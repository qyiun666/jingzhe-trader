package scheduler

// ETF 补口的配置读取与接入点（策略侧改动面第 4/6 项）。
//
// 三键的归属：`screen.etf_enabled` / `screen.etf_whitelist` /
// `screen.etf_min_avg_amount_yi` 由工程在 config.KeySpec 落键（默认
// false / 空 / 2.0），本文件只负责读取与钳回，不再重复一份默认值字面量。
//
// 为什么 ETF 是"补口"而不是"并级"：设计文档实测 13 只科技 ETF 的 20 日动量
// → 未来 20 日 RankIC 为 −0.019，且前后两半符号相反（−0.038 / +0.001），池内
// 两两相关最高 0.985 —— 没有横截面信息可排，也就不能塞进个股那个
// PercentileRank 截面（会扭曲已经验证过的个股分数）。因此这里独立跑一条
// 两级小漏斗，再把候选并入同一个决策批次交给 LLM 评审。

import (
	"context"
	"strconv"
	"strings"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
)

// 规模下限不在这里：工程的 dataloader 用 fund_share × 未复权收盘做运行期校验
// （dataloader.ETFMinScaleYi = 50 亿，低于即 alert:ETF_SCALE 出声但不剔），
// 本侧曾留过一个同值的"挑白名单时的一次性人工复核项"常量，两处 50 亿迟早漂移，
// 删掉它、以数据链路那一份为准。

// ETFOptionsOf 由配置构建 ETF 小漏斗的运行口径。读取纪律与 MarketGateOf 一致：
// 键缺失的旧库按"未启用"处理（默认关闭 = 与上线前逐位一致），坏值按更严的一档回落。
func ETFOptionsOf(cfg ConfigReader) screener.ETFOptions {
	enabled, err := strconv.ParseBool(strings.TrimSpace(cfg.GetString("screen.etf_enabled")))
	if err != nil {
		enabled = false
	}
	yi, err := strconv.ParseFloat(strings.TrimSpace(cfg.GetString("screen.etf_min_avg_amount_yi")), 64)
	if err != nil || yi < 0 {
		// 坏值不等于"没有下限"：取 KeySpec 的默认档 2.0 亿，宁可少放候选。
		yi = etfDefaultMinAmountYi
	}
	return screener.ETFOptions{
		Enabled:         enabled,
		Pool:            screener.ParseETFWhitelist(cfg.GetString("screen.etf_whitelist")),
		MinAvgAmountFen: model.FromFloat(yi * 1e8), // 亿元 → 元 → Fen(分)
		Window:          screener.BarWindow(),
	}
}

// etfDefaultMinAmountYi 与工程在 KeySpec 里给 screen.etf_min_avg_amount_yi 的默认值一致。
// 这里只兜"键存在但值坏了"的情况；键缺失时同样落到这个数，因为 Enabled=false 时它不参与任何判定。
const etfDefaultMinAmountYi = 2.0

// ETFUniverseOf 白名单声明的 ETF 代码集（ts_code → 名称），注入卖出链。
//
// 有意**不看 etf_enabled**：停掉补口只是"今天不再买 ETF"，手里已有的仓位还在，
// 而它需要的是"不受排名淘汰约束 + 名称可解析"这两条。把 universe 和开关绑在一起，
// 等于关开关的那晚把存量 ETF 无条件清仓。
func ETFUniverseOf(cfg ConfigReader) signal.ETFUniverse {
	u := signal.ETFUniverse{}
	for _, in := range screener.ParseETFWhitelist(cfg.GetString("screen.etf_whitelist")) {
		if in.Name != "" {
			u[in.TsCode] = in.Name
		}
	}
	if len(u) == 0 {
		return nil
	}
	return u
}

// etfShouldRun 补口的启用判据：**仅弱势试探期**。
//
// 组长批准的口径逐字落到这里：gate_enabled=false 且弱势判定为真。第二个条件
// 在 ScreenBudget 里只有在闸门关闭时才会置位，所以两条一起写是冗余的——
// 冗余是故意的：这个函数的存在就是为了那条验收标准
// （etf_enabled=true 但 gate=true 时不得产出 ETF 候选），
// 把隐含依赖摊成显式条件，将来 ScreenBudget 改了置位逻辑也不会悄悄放行。
func etfShouldRun(opts screener.ETFOptions, gate store.MarketGate, budget screener.Budget) bool {
	return opts.Enabled && !gate.Enabled && budget.WeakRegime
}

// appendETFCandidates 弱势期跑 ETF 补口并把候选并入个股批次（不排名、不打分）。
//
// 失败只降级不断链：ETF 是给"科技股一手买不起"开的补口，某天 fund_daily 没同步
// 成功，正确反应是个股链照常走、当日少一个候选来源，而不是整条 evening_pipeline
// 失败。这里唯一会向上抛的是"配置说启用却读不到数"——那要出声，否则补口静默失效
// 与从没启用过看起来一模一样（历史上 gate 停摆就是这么拖了两周的）。
func appendETFCandidates(ctx context.Context, rc *observability.RunCtx, d Deps, date string,
	cands []model.Candidate, budget screener.Budget) []model.Candidate {
	opts := ETFOptionsOf(d.Config)
	if !etfShouldRun(opts, MarketGateOf(d.Config), budget) {
		return cands
	}
	etfRep, err := screener.RunETF(ctx, d.Store.ScreenRepo(), opts, budget, date)
	if err != nil {
		msg := "ETF 补口读取失败，当日只用个股候选继续：" + err.Error()
		rc.Degrade("ETF_UNAVAILABLE", msg)
		d.raiseETFTrace(ctx, date, "ETF_UNAVAILABLE", msg)
		observability.S().Warnw("ETF 补口未执行", "date", date, "err", err.Error())
		return cands
	}
	for _, st := range etfRep.Stages {
		key := "screen_" + st.Slug
		rc.Declare("rows", key, 0) // 期望 0：白名单全被剔是可归因的正常结论
		rc.Actual(key, st.Out)
	}
	for _, n := range etfRep.Notes {
		observability.S().Infow("ETF 补口提示", "date", date, "note", n)
	}
	if etfRep.Empty() {
		msg := "ETF 补口已启用且判定为弱势，但白名单无一入围：" + etfRep.Summary()
		rc.Degrade("ETF_EMPTY", msg)
		d.raiseETFTrace(ctx, date, "ETF_EMPTY", msg)
		return cands
	}
	observability.S().Infow("ETF 补口产出候选", "date", date,
		"pool", len(opts.Pool), "candidates", len(etfRep.Candidates), "funnel", etfRep.Summary())
	return append(cands, etfRep.Candidates...)
}

// raiseETFTrace 补口的异常单独落一行 alert:<code>（与 alert:WEAK_REGIME 同口径）：
// 只写在整条 job 的 detail 里的话，事后按日期捞不到"哪天补口没跑、为什么"。
// 写失败不影响交易链路，但必须留日志。
func (d Deps) raiseETFTrace(ctx context.Context, date, code, detail string) {
	trace := model.RunTrace{
		TradeDate: date, Subject: model.TraceAlert(code),
		Outcome: model.TracePartial,
		Detail:  date + " ETF 补口：" + detail,
		At:      time.Now().UTC().Format(time.RFC3339),
	}
	if err := d.Store.TraceRepo().Write(ctx, trace); err != nil {
		observability.S().Errorw("写 ETF 补口轨迹失败", "date", date, "code", code, "err", err.Error())
	}
}
