package scheduler

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
)

// 本文件钉住 2026-11-03 亏损复盘定下的两条调度侧口径：
//   [P0] 盘中扫描不得用瞬时现价抬升期间最高价（高点只归 EOD 收盘路径维护）。
//   [P1] 流水线任务级超时随评审链整轮预算自动扩，两者不再各自为政。

// stubCfg 只读配置替身（scheduler.ConfigReader 的唯一方法）。
type stubCfg map[string]string

func (s stubCfg) GetString(key string) string { return s[key] }

// TestIntradayScanNeverRaisesHighPrice 旧实现每 5 分钟 RaiseHighPrice(现价)，只升不降，
// 一根上影线就能永久武装移动止盈基准 —— 20261103 601328 的成本 8.26、盘中高点 8.76
// 仅高出 6% 武装线 0.0044 元，次日低开穿破触发价 8.322、8.13 成交，+3.75% 浮盈翻成
// -2.28% 实现（其中执行滑价就占 -19.2 元）。
//
// 用例按同一形状反着钉：现价先冲 +10%（不触 15% 止盈），再回到 -4%。
//   - 旧实现：高点被抬到 11.00 ⇒ 触发线 11.00×0.95=10.45，现价 9.60 已穿破，且
//     11.00 ≥ 成本×1.06（已武装）→ 第二轮就出一张"移动止盈"卖单。
//   - 新实现：高点停在 10.00 ⇒ 10.00 < 10.60 永不武装，触发线 9.50 也没被穿破 → 零张单。
//
// 因此本例同时验证"没抬高点"与"没有误卖"，改回旧行为会在这里炸。
func TestIntradayScanNeverRaisesHighPrice(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/highsrc.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()

	const code, date = "600000.SH", "20261104"
	if err := st.MarketRepo().UpsertCal(ctx, store.CalRow{CalDate: date, IsOpen: true}); err != nil {
		t.Fatalf("预置交易日历失败: %v", err)
	}
	if err := st.TradeRepo().UpsertPosition(ctx, model.Position{
		TsCode: code, TotalQty: 100, CostPrice: model.FromFloat(10),
		HighPrice: model.FromFloat(10), FirstOpenDate: "20261101",
	}); err != nil {
		t.Fatalf("预置持仓失败: %v", err)
	}

	high := func() model.Fen {
		t.Helper()
		p, err := st.TradeRepo().GetPosition(ctx, code)
		if err != nil {
			t.Fatalf("读持仓失败: %v", err)
		}
		return p.HighPrice
	}

	d := Deps{Store: st}
	rc := observability.NewRunCtx(ctx, "intraday_scan", date)
	rp := risk.RiskParams{
		StopLossPct: 0.08, TrailingStopPct: 0.05, TrailingArmPct: 0.06, TakeProfitPct: 0.15,
	}

	scan := func(px float64) int {
		t.Helper()
		lines, err := stopLossTickets(ctx, rc, d, date, []string{code},
			map[string]quote.Quote{code: {TsCode: code, Price: model.FromFloat(px)}}, rp, nil)
		if err != nil {
			t.Fatalf("盘中扫描（现价 %.2f）失败: %v", px, err)
		}
		return len(lines)
	}

	// 第一轮：冲高 +10%。任何规则都不该触发，且高点不得被抬起来。
	if got := scan(11.00); got != 0 {
		t.Errorf("现价 11.00（+10%%，未及止盈线）不应产生卖单，实际 %d 张", got)
	}
	if got := high(); got != model.FromFloat(10) {
		t.Errorf("盘中现价不得抬升期间最高价：high_price=%d 分，期望仍为 %d 分（收盘口径值）",
			int64(got), int64(model.FromFloat(10)))
	}

	// 第二轮：回落到 -4%。高点没被抬 → 移动止盈未武装 → 依旧零张单。
	if got := scan(9.60); got != 0 {
		t.Errorf("现价 9.60（-4%%）不应触发移动止盈（旧实现会在这里卖飞），实际 %d 张", got)
	}
	if got := high(); got != model.FromFloat(10) {
		t.Errorf("两轮扫描后 high_price 仍应停在收盘口径 %d 分，实际 %d 分",
			int64(model.FromFloat(10)), int64(got))
	}

	// 正控制：证明上面的断言不是"读不到就恒等"。同一个仓储方法由 EOD 路径调用时
	// 必须仍然抬升高点（pipeline.go 的 raiseHighWatermarks 用它写收盘价），
	// 删掉的只是盘中那一处调用，不是高点维护本身。
	if err := st.TradeRepo().RaiseHighPrice(ctx, code, model.FromFloat(11)); err != nil {
		t.Fatalf("正控制调用 RaiseHighPrice 失败: %v", err)
	}
	if got := high(); got != model.FromFloat(11) {
		t.Fatalf("正控制失效：EOD 高点维护路径没有写入（high_price=%d 分，期望 1100 分），"+
			"说明前面的断言读的根本不是持仓表", int64(got))
	}

	// 下跌保护没有被削弱：8% 硬止损与 15% 止盈都只读成本线，不使用期间高点
	// （signal/sell.go 里 HighPrice 只出现在 evalTrailingStop），所以删除盘中抬升
	// 影响的只有"移动止盈被瞬时高点骗武装"这一条路径。规则侧的武装口径由
	// signal 包的 TestSellTrailingStopArm 钉住，本例只钉调度侧不再写高点。
}

// TestPipelineTimeoutFollowsReviewDeadline 评审链整轮预算被调大时，流水线任务级超时
// 必须跟着扩：否则调度器先掐死任务，评审的失败行与 LLM_FAILED 降级来不及落库
// （agent_issues #14 的 20261030：20 只候选 value 阶段整体消失，当天轨迹看不出原因）。
func TestPipelineTimeoutFollowsReviewDeadline(t *testing.T) {
	pipelineTimeout := func(cfg stubCfg) int {
		t.Helper()
		for _, j := range BuildJobs(Deps{Config: cfg}) {
			if j.Name == JobEveningPipeline {
				return int(j.Timeout.Minutes())
			}
		}
		t.Fatalf("BuildJobs 未产出 %s 任务", JobEveningPipeline)
		return 0
	}

	if got := pipelineTimeout(stubCfg{"scheduler.pipeline": "16:30"}); got != 30 {
		t.Errorf("未配置评审预算时流水线超时应保持原来的 30 分钟，实际 %d", got)
	}
	// 评审整轮被调到 40 分钟（> 30 - 5 的余量线）→ 任务超时自动扩到 45 分钟。
	if got := pipelineTimeout(stubCfg{"scheduler.pipeline": "16:30", "llm.review_deadline_min": "40"}); got < 45 {
		t.Errorf("评审整轮 40 分钟时流水线超时须 ≥45 分钟（含 5 分钟建单/轨迹/发信余量），实际 %d", got)
	}
	// 坏值（0/负数/非数字）不静默把超时缩回 0，回落默认 30 分钟下限。
	for _, bad := range []string{"", "0", "-5", "abc"} {
		if got := pipelineTimeout(stubCfg{"scheduler.pipeline": "16:30", "llm.review_deadline_min": bad}); got != 30 {
			t.Errorf("llm.review_deadline_min=%q 应落回 30 分钟下限，实际 %d", bad, got)
		}
	}
}
