package scheduler

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// eveningPipeline 收盘后整链大方法：一条顺序流水线，任一步失败即整链失败（落 run_trace outcome=fail）。
//
// ① 行情同步 → ② 新鲜度门禁 → ③ 季度档位 → ④ 选股漏斗 → ⑤ 买卖决策 → ⑥ 写待买卖表。
// 只有 ①③⑥ 写库：行情是缓存、档位是一行状态、指令单是结果；④⑤ 的中间产物全部在内存里传递，
// 每级进出计数写日志。
func eveningPipeline(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	if err := syncTodayBars(ctx, rc, d, date); err != nil {
		return fmt.Errorf("① 行情同步: %w", err)
	}
	if err := gateFreshness(ctx, d, date); err != nil {
		d.raiseU(rc, "DATA_STALE", "数据不新鲜，当日不出指令", err.Error())
		return fmt.Errorf("② 新鲜度门禁: %w", err)
	}
	if err := evaluateGear(ctx, rc, d, date); err != nil {
		return fmt.Errorf("③ 档位评估: %w", err)
	}
	rp, gear, err := d.RiskParams(ctx, date)
	if err != nil {
		return fmt.Errorf("读取生效风控参数失败: %w", err)
	}
	cands, err := screenCandidates(ctx, rc, d, date, rp)
	if err != nil {
		return fmt.Errorf("④ 选股: %w", err)
	}
	if err := buildTickets(ctx, rc, d, date, cands, rp, gear); err != nil {
		return fmt.Errorf("⑤ 买卖决策: %w", err)
	}
	return nil
}

// syncTodayBars 刷新在市股票清单 + 拉取当日行情，并把选股窗口内的缺口一并补齐。
//
// 回补天数由选股器给出（它是最深消费者）：以前固定回补 10 天而因子窗口要 20 根，
// 新库永远凑不满窗口。每补一天 = 每个接口一次调用（返回全市场），不是逐只调用。
func syncTodayBars(ctx context.Context, rc *observability.RunCtx, d Deps, date string) error {
	// 日历必须在日线之前：SyncDaily 靠日历挑"回补哪几天"，空日历会让它算出 0 个日期，
	// 于是一行日线都不写却照样返回成功。
	if err := renewCalendar(ctx, d, date); err != nil {
		return fmt.Errorf("交易日历补齐失败: %w", err)
	}
	if err := d.Dataloader.SyncStockBasics(ctx); err != nil {
		return fmt.Errorf("股票清单同步失败: %w", err)
	}
	rc.Declare("rows", "daily_bar", -1)
	if err := d.Dataloader.SyncDaily(ctx, date, d.Screener.SyncBackDays()); err != nil {
		return err
	}
	n, err := d.Store.MarketRepo().CountBar(ctx, date)
	if err != nil {
		return fmt.Errorf("核对日线行数失败: %w", err)
	}
	rc.Actual("daily_bar", n)
	return nil
}

// gateFreshness 数据新鲜度门禁。不新鲜返回 error → 整链中止，当日不出任何指令。
//
// 这是"下游不得跑在上游之前"的唯一收口点：行情没出全时以前会让选股在旧数据上
// degraded 定稿，而调度器把 degraded 视作当日已完成，后续不再重跑。
func gateFreshness(ctx context.Context, d Deps, date string) error {
	rep, err := d.Freshness.Check(ctx, date)
	if err != nil {
		return fmt.Errorf("门禁执行失败: %w", err)
	}
	if !rep.Fresh {
		return fmt.Errorf("数据不新鲜: %s", rep.String())
	}
	return nil
}

// evaluateGear 季度档位评估：总资产由 goal 经注入的资产源实时推算（不落库），
// 结果只更新档位状态这一个键（config_kv 的 goal.state）。
func evaluateGear(ctx context.Context, rc *observability.RunCtx, d Deps, date string) error {
	rc.Declare("state", "goal", -1)
	res, err := d.Goal.Evaluate(ctx, date)
	if err != nil {
		return fmt.Errorf("季度档位评估失败: %w", err)
	}
	rc.Actual("goal", 1)
	observability.S().Infow("档位评估完成",
		"date", date, "gear", string(res.Decision.To), "changed", res.Decision.Changed,
		"reason", res.Decision.Reason, "progress_pct", res.Metrics.Progress)
	return nil
}

// screenCandidates 跑选股漏斗（全程内存），返回进入决策链的候选。
func screenCandidates(ctx context.Context, rc *observability.RunCtx, d Deps, date string,
	rp risk.RiskParams) ([]model.Candidate, error) {
	budget, err := ScreenBudget(ctx, d.Store, d.Ledger, date, rp)
	if err != nil {
		return nil, err
	}
	rep, err := d.Screener.Run(ctx, date, budget)
	if err != nil {
		return nil, err
	}
	rc.Declare("rows", "candidates", -1)
	rc.Actual("candidates", len(rep.Candidates))
	// 每一级漏斗的存量都写进运行日志：期望 0，因此不会因"筛空"被判缺失。
	for _, st := range rep.Stages {
		key := "screen_" + st.Slug
		rc.Declare("rows", key, 0)
		rc.Actual(key, st.Out)
	}
	if rep.Empty {
		rc.Degrade("SCREEN_EMPTY", "候选 0 条，卡在哪一级见上方漏斗行")
	}
	return rep.Candidates, nil
}

// ScreenBudget 组装选股漏斗的资金与大盘口径：
// 单笔预算 = 可用现金 / 计划持仓数；大盘跌破 MA20 时当日关闭买入漏斗。
//
// 现金与指数都是这道闸门必需 inputs：拿不到就失败，不存在"本轮先不判定大盘"——
// 那等于在最该保守的时候默认放行买入。
//
// 调度器与 `jingzhe run task screen` 共用这一个实现：两边各写一套判据，
// 手工复现出的漏斗就与到点自动跑的不一致（历史上 CLI 那套把 MarketOK 写死为 true）。
func ScreenBudget(ctx context.Context, st *store.Store, led *ticket.Ledger,
	date string, rp risk.RiskParams) (screener.Budget, error) {
	b := screener.Budget{Slots: rp.MaxPositions}
	ast, err := led.Assets(ctx, date)
	if err != nil {
		return b, fmt.Errorf("读取账户现金失败，可用资金筛无法判定: %w", err)
	}
	b.Cash = ast.Cash
	idx, err := st.ScreenRepo().LatestMarketIndex(ctx, date)
	if err != nil {
		return b, err
	}
	if idx.MA20 <= 0 {
		return b, fmt.Errorf("大盘指数 %s 在 %s 前不足 20 根日线，MA20 不可算", store.MarketIndex, date)
	}
	b.MarketOK = idx.Close >= idx.MA20
	return b, nil
}

// buildTickets 由候选与持仓生成买卖决策并写入待买卖表（drafted，等 17:00 发邮件）。
//
// 买入决策权在 LLM 评审员手里，因此 llm.enabled=false 不是"少一道终审"，而是"当日不可能有买单"——
// 这一点必须显式告警，否则人会以为流水线跑通了却什么都没买到。
func buildTickets(ctx context.Context, rc *observability.RunCtx, d Deps, date string,
	cands []model.Candidate, rp risk.RiskParams, gear model.Gear) error {
	if !d.Decider.Enabled() {
		d.raiseU(rc, "LLM_DISABLED", "买入决策未启用，当日不会有任何买单",
			"llm.enabled=false 或 api_key/model 缺失；风控参数不会替代决策者")
	}
	rep, err := d.Signal.Generate(ctx, date, cands, rp, gear, d.Decider)
	if err != nil {
		return err
	}
	rc.Declare("rows", "pending_tickets", 0) // 期望 0：评审后决定都不买是正常结果，不是缺产出
	rc.Actual("pending_tickets", rep.Tickets)
	rc.Declare("rows", "llm_declined", 0) // 期望 0：评审否决是正常工作结果，不是缺失
	rc.Actual("llm_declined", rep.Declined)
	for _, n := range rep.Notes {
		observability.S().Infow("决策阶段提示", "date", date, "note", n)
	}
	observability.S().Infow("买卖决策完成", "date", date, "candidates", rep.Candidates,
		"approved", rep.Approved, "declined", rep.Declined, "review_failed", rep.Failed,
		"sell", rep.SellSignals, "rejected", rep.Rejected,
		"tickets", rep.Tickets, "skipped_existing", rep.Skipped)
	if rep.Failed > 0 {
		msg := fmt.Sprintf("%d 只候选评审未问出结果，明细见当日轨迹的 llm:* 失败行", rep.Failed)
		d.raiseU(rc, "LLM_FAILED", "买入评审部分失败，当日这些标的不建仓", msg)
		rc.Degrade("LLM_FAILED", msg)
	}
	if rep.Rejected > 0 && rep.Tickets == 0 && rep.Approved+rep.SellSignals > 0 {
		d.raiseU(rc, "ALL_REJECTED", "有决策但全被风控否决", fmt.Sprintf("否决 %d 条", rep.Rejected))
		rc.Degrade("ALL_REJECTED", fmt.Sprintf("否决 %d 条", rep.Rejected))
	}
	return nil
}
