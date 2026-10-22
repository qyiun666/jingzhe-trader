package signal

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
)

// g1 生效参数（止损 8% / 移动止盈 5% / 止盈 15%）。
func g1() risk.RiskParams { return risk.DefaultParams(model.FromFloat(100000)) }

// holding 构造持仓上下文。
func holding(cost, high, last model.Fen, inTopN, marketBad bool) HoldingCtx {
	return HoldingCtx{
		Pos: model.Position{
			TsCode: "sh600001", TotalQty: 1000,
			CostPrice: cost, HighPrice: high,
		},
		LastClose: last,
		InTopN:    inTopN, MarketBad: marketBad,
	}
}

// TestSellStopLoss 规则 1 止损：收盘跌破止损线触发；未跌破不触发。
func TestSellStopLoss(t *testing.T) {
	p := g1()
	// 成本 10 元，止损线 = 9.20 元
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(9.2), true, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleStopLoss {
		t.Fatalf("应触发止损: %+v", sig)
	}
	if sig.Direction != model.DirSell {
		t.Errorf("方向=%s, 期望 sell", sig.Direction)
	}
	if !strings.Contains(sig.Reason, "止损") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(9.21), true, false), p, 0, 0) != nil {
		t.Errorf("9.21 元未破止损线，不应触发")
	}
}

// TestSellTrailingStop 规则 2 移动止盈：自高点回撤超线且已有盈利时触发。
func TestSellTrailingStop(t *testing.T) {
	p := g1()
	// 成本 10 元，高点 12 元，回撤线 = 12×0.95 = 11.40
	sig := EvalSell("20260901", holding(model.FromFloat(10), model.FromFloat(12), model.FromFloat(11.4), true, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleTrailingStop {
		t.Fatalf("应触发移动止盈: %+v", sig)
	}
	if !strings.Contains(sig.Reason, "回撤") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	// 11.41 未破回撤线 → 不触发
	if EvalSell("20260901", holding(model.FromFloat(10), model.FromFloat(12), model.FromFloat(11.41), true, false), p, 0, 0) != nil {
		t.Errorf("未破回撤线不应触发")
	}
	// 高点低于成本（未盈利过）→ 不做移动止盈，回落走止损规则
	// （成本 12，止损线 11.04，现价 10.40 已破线 → 期望 stop_loss）
	if sig := EvalSell("20260901", holding(model.FromFloat(12), model.FromFloat(11), model.FromFloat(10.4), true, false), p, 0, 0); sig == nil || sig.Rule != RuleStopLoss {
		t.Errorf("高点未过成本不应触发移动止盈，应回落止损: %+v", sig)
	}
	// 高点高于成本但未破回撤线 → 不触发任何规则
	if sig := EvalSell("20260901", holding(model.FromFloat(12), model.FromFloat(13), model.FromFloat(12.5), true, false), p, 0, 0); sig != nil {
		t.Errorf("盈利未回撤不应触发: %+v", sig)
	}
}

// TestSellTrailingStopArm 移动止盈必须先"涨够"才武装（risk.trailing_arm_pct）：
// 高点未达成本×(1+arm) 时，下跌只走 8% 止损，不再被 5% 回撤线提前砍掉。
func TestSellTrailingStopArm(t *testing.T) {
	p := g1() // TrailingStopPct=0.05、TrailingArmPct=0.06
	// 高=成本、现价 -5%：移动止盈未武装；8% 止损线 9.20 未破 → 不卖
	if sig := EvalSell("20261022", holding(model.FromFloat(10), model.FromFloat(10), model.FromFloat(9.5), true, false), p, 0, 0); sig != nil {
		t.Fatalf("高点未达成本×1.06，移动止盈不应触发，实际 %+v", sig)
	}
	// 高=成本×1.10、自高点回撤 5% → 触发移动止盈
	sig := EvalSell("20261022", holding(model.FromFloat(10), model.FromFloat(11), model.FromFloat(10.45), true, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleTrailingStop {
		t.Fatalf("高点已过激活线且回撤 5%%，应触发移动止盈，实际 %+v", sig)
	}
	// 高=成本×1.02、现价成本 -6%：未武装，8% 止损线 9.20 未破 → 不卖（交给止损规则）
	if sig := EvalSell("20261022", holding(model.FromFloat(10), model.FromFloat(10.2), model.FromFloat(9.4), true, false), p, 0, 0); sig != nil {
		t.Fatalf("高点未过激活线、止损线未破，不应卖出，实际 %+v", sig)
	}
	// 高=成本×1.02、跌破 8% 止损 → 走止损，不是移动止盈
	sig = EvalSell("20261022", holding(model.FromFloat(10), model.FromFloat(10.2), model.FromFloat(9.1), true, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleStopLoss {
		t.Fatalf("跌破 8%% 止损线应走止损，实际 %+v", sig)
	}
}

// TestTrailingStopReplay20261022 用 10-22 600704 的真实数据回放：
// 成本/期间高 5.07、收盘 4.81。旧口径 highGate=5.07×0.95=4.8165 会触发
// "移动止盈"（当天实际就是这样卖出的）；新口径高点未达 5.07×1.06=5.3742，
// 8% 止损线 4.6644 未破，持有 8 个交易日 <10 日排名淘汰豁免 → 不应出卖单。
func TestTrailingStopReplay20261022(t *testing.T) {
	p := risk.DefaultParams(model.FromFloat(11800))
	h := HoldingCtx{
		Pos:       model.Position{TsCode: "600704.SH", TotalQty: 200, CostPrice: 507, HighPrice: 507},
		LastClose: 481, LastDate: "20261022", InTopN: false,
		DaysHeld: 8, RankOut: RankOutPolicy{MinHoldDays: 10, WatchTopN: 30},
	}
	if sig := EvalSell("20261022", h, p, 0, 0); sig != nil {
		t.Fatalf("新激活线下 600704 不应卖出（旧口径会在 4.8165 触发移动止盈），实际 %+v", sig)
	}
}

// TestSellRankOutNoBand 空 band 是"无榜"不是"跌出榜"：规则 4 跳过；
// 关闸日（指数在均线下方）仍由规则 5 退出；有 band 时旧口径回归。
func TestSellRankOutNoBand(t *testing.T) {
	p := g1()
	ro := RankOutPolicy{MinHoldDays: 10, WatchTopN: 30}
	h := rankOutHold(30, ro)
	h.NoBand = true

	if sig := EvalSell("20261021", h, p, 0, 0); sig != nil {
		t.Fatalf("空 band 且非关闸日不应排名淘汰，实际 %+v", sig)
	}
	sig := EvalSell("20261021", h, p, model.FromFloat(3000), model.FromFloat(3100))
	if sig == nil || sig.Rule != RuleMarketBad {
		t.Fatalf("空 band 时关闸日仍应由规则 5 退出，实际 %+v", sig)
	}
	h.NoBand = false
	if sig := EvalSell("20261021", h, p, 0, 0); sig == nil || sig.Rule != RuleRankOut {
		t.Fatalf("有 band 时持仓跌出榜应照旧排名淘汰，实际 %+v", sig)
	}
}

// TestSellTakeProfit 规则 3 止盈：收盘达到止盈线触发。
func TestSellTakeProfit(t *testing.T) {
	p := g1()
	// 成本 10 元，止盈线 = 11.50 元
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(11.5), true, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleTakeProfit {
		t.Fatalf("应触发止盈: %+v", sig)
	}
	if !strings.Contains(sig.Reason, "止盈") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(11.49), true, false), p, 0, 0) != nil {
		t.Errorf("11.49 元未到止盈线不应触发")
	}
}

// TestSellRankOut 规则 4 排名淘汰：未进入当日候选池 TopN 触发。
func TestSellRankOut(t *testing.T) {
	p := g1()
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), false, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleRankOut {
		t.Fatalf("应触发排名淘汰: %+v", sig)
	}
	// 在候选池 → 不触发
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false), p, 0, 0) != nil {
		t.Errorf("在候选池不应触发排名淘汰")
	}
}

// rankOutHold 构造一只"跌出保留带、持有 DaysHeld 个交易日"的个股持仓。
func rankOutHold(daysHeld int, ro RankOutPolicy) HoldingCtx {
	h := holding(model.FromFloat(10), 0, model.FromFloat(10), false, false)
	h.DaysHeld = daysHeld
	h.RankOut = ro
	return h
}

// TestSellRankOutMinHold 最小持有期（signal.rank_out_min_hold_days，2026-10-14 组长批准）：
// 持有未满 → 排名淘汰豁免；满 → 照常淘汰；持有期未知（-1，老同步持仓缺日期）→
// 不适用保护（缺数据不是锁仓理由）；MinHoldDays=0 → 旧口径逐位一致。
func TestSellRankOutMinHold(t *testing.T) {
	p := g1()
	ro := RankOutPolicy{MinHoldDays: 10, WatchTopN: 30}
	if sig := EvalSell("20260901", rankOutHold(5, ro), p, 0, 0); sig != nil {
		t.Errorf("持有 5 交易日 < 10，应豁免排名淘汰，实际 %+v", sig)
	}
	if sig := EvalSell("20260901", rankOutHold(9, ro), p, 0, 0); sig != nil {
		t.Errorf("持有 9 交易日 < 10（边界内一格），应豁免，实际 %+v", sig)
	}
	sig := EvalSell("20260901", rankOutHold(10, ro), p, 0, 0)
	if sig == nil || sig.Rule != RuleRankOut {
		t.Fatalf("持有 10 交易日 ≥ 10，应触发淘汰: %+v", sig)
	}
	if !strings.Contains(sig.Reason, "Top30") {
		t.Errorf("理由应写明保留带深度，实际 %q", sig.Reason)
	}
	if sig := EvalSell("20260901", rankOutHold(-1, ro), p, 0, 0); sig == nil || sig.Rule != RuleRankOut {
		t.Errorf("持有期未知不适用保护，应触发淘汰: %+v", sig)
	}
	// 旧口径回归：MinHoldDays=0 时不管持有几天都照淘汰（与开关前逐位一致）。
	if sig := EvalSell("20260901", rankOutHold(1, RankOutPolicy{}), p, 0, 0); sig == nil || sig.Rule != RuleRankOut {
		t.Errorf("MinHoldDays=0 应回到旧口径: %+v", sig)
	}
}

// TestSellRankOutMinHoldKeepsPriceRules 持有期只挡"排名换仓"：未满持有期不豁免
// 止损/移动止盈/止盈——建仓次日跌破止损线照样出信号（风控优先原则的回归点）。
func TestSellRankOutMinHoldKeepsPriceRules(t *testing.T) {
	p := g1() // 止损线 8%
	h := rankOutHold(1, RankOutPolicy{MinHoldDays: 10})
	h.LastClose = model.FromFloat(9.0) // 成本 10，现价 9.0 → 破 9.2 止损线
	sig := EvalSell("20260901", h, p, 0, 0)
	if sig == nil || sig.Rule != RuleStopLoss {
		t.Fatalf("持有期保护不该挡止损，实际 %+v", sig)
	}
}

// TestTradingDaysHeld 持有交易日数：建仓日不计；日历缺端点仍按数出来的算；
// 空建仓日期/晚于判定日返回 -1（保护不适用哨兵）。
func TestTradingDaysHeld(t *testing.T) {
	days := []string{"20261008", "20261009", "20261012", "20261013", "20261014"}
	cases := []struct {
		open, date string
		want       int
	}{
		{"20261008", "20261008", 0}, // 建仓当日 = 0 个持有交易日（T+1 不可卖口径）
		{"20261008", "20261009", 1},
		{"20261008", "20261014", 4},  // 跳过周末非交易日
		{"20261012", "20261014", 2},  // 真实样本：600704 建仓 10/12 → 10/14 持 2 日
		{"20261013", "20261015", 1},  // 判定日不在日历：只数日历内的
		{"", "20261014", -1},         // 缺建仓日期
		{"20261016", "20261014", -1}, // 坏数据：建仓晚于判定日
	}
	for _, c := range cases {
		if got := tradingDaysHeld(days, c.open, c.date); got != c.want {
			t.Errorf("tradingDaysHeld(%q→%q)=%d，期望 %d", c.open, c.date, got, c.want)
		}
	}
}

// TestSellMarketBad 规则 5 大盘恶化：指数收盘跌破 MA60 触发。
func TestSellMarketBad(t *testing.T) {
	p := g1()
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false), p,
		model.FromFloat(3000), model.FromFloat(3100))
	if sig == nil || sig.Rule != RuleMarketBad {
		t.Fatalf("应触发大盘恶化: %+v", sig)
	}
	if !strings.Contains(sig.Reason, "MA60") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	// 指数在 MA60 之上 → 不触发
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false), p,
		model.FromFloat(3200), model.FromFloat(3100)) != nil {
		t.Errorf("指数未破 MA60 不应触发")
	}
	// 指数数据缺失（close=0）→ 不判定
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false), p, 0, 0) != nil {
		t.Errorf("指数缺失不应触发大盘恶化")
	}
}

// TestSellPriority 优先级：同一持仓同时满足多条时只出首条（止损优先）。
func TestSellPriority(t *testing.T) {
	p := g1()
	// 同时满足止损（跌破 9.2）与排名淘汰
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(9), false, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleStopLoss {
		t.Fatalf("止损应优先: %+v", sig)
	}
}

// weakExitCtx 构造弱势减仓的持仓上下文（成本/现价 10 元，在榜内避免规则 4）。
func weakExitCtx(qty model.Qty, drop float64) HoldingCtx {
	return HoldingCtx{
		Pos:       model.Position{TsCode: "600000.SH", TotalQty: qty, CostPrice: 1000, HighPrice: 1000},
		LastClose: 1000, InTopN: true,
		IndexBelowMA: true, IndexRet5: drop,
		WeakExit: WeakExitPolicy{Enabled: true, DropPct: 0.03, ReducePct: 0.5},
	}
}

// TestSellWeakExit 规则 6 弱势减仓：指数在均线下方且 5 日跌幅达阈值 → 减 50%
// 向下取整整手；100 股无法减出一手、跌幅不足、指数在均线上方均不触发。
func TestSellWeakExit(t *testing.T) {
	p := g1()

	sig := EvalSell("20261023", weakExitCtx(200, -0.05), p, 0, 0)
	if sig == nil || sig.Rule != RuleWeakExit {
		t.Fatalf("200 股 + 5 日 -5%% 应触发弱势减仓，实际 %+v", sig)
	}
	if sig.Qty != 100 {
		t.Errorf("200 股减 50%% 应卖 100 股，实际 %d", int64(sig.Qty))
	}
	if !strings.Contains(sig.Reason, "只减不清") {
		t.Errorf("理由应写明只减不清: %q", sig.Reason)
	}

	// 250 股 × 50% = 125 → 向下取整到 100
	if sig := EvalSell("20261023", weakExitCtx(250, -0.05), p, 0, 0); sig == nil || sig.Qty != 100 {
		t.Errorf("250 股应减 100 股（向下取整整手），实际 %+v", sig)
	}
	// 100 股 × 50% = 50 → 不足一手，跳过（仍由止损/移动止盈/止盈管理）
	if sig := EvalSell("20261023", weakExitCtx(100, -0.05), p, 0, 0); sig != nil {
		t.Errorf("100 股不足一手应跳过，实际 %+v", sig)
	}
	// 跌幅未达阈值
	if sig := EvalSell("20261023", weakExitCtx(200, -0.02), p, 0, 0); sig != nil {
		t.Errorf("5 日 -2%% 未达 3%% 阈值，不应触发，实际 %+v", sig)
	}
	// 指数在均线上方
	h := weakExitCtx(200, -0.05)
	h.IndexBelowMA = false
	if sig := EvalSell("20261023", h, p, 0, 0); sig != nil {
		t.Errorf("指数未破均线不应触发弱势减仓，实际 %+v", sig)
	}
	// 策略关闭
	h = weakExitCtx(200, -0.05)
	h.WeakExit.Enabled = false
	if sig := EvalSell("20261023", h, p, 0, 0); sig != nil {
		t.Errorf("弱势减仓关闭时不应触发，实际 %+v", sig)
	}
	// 止损优先：同一天跌破 8% 止损线时不能只减半仓
	h = weakExitCtx(200, -0.05)
	h.LastClose = model.FromFloat(9)
	if sig := EvalSell("20261023", h, p, 0, 0); sig == nil || sig.Rule != RuleStopLoss || sig.Qty != 0 {
		t.Errorf("止损应优先且按可卖量全出，实际 %+v", sig)
	}
}

// TestWeakExitQtyRespectsT1 减仓数量还受 T+1 可卖量约束：算出一手但当日可卖不足时跳过。
func TestWeakExitQtyRespectsT1(t *testing.T) {
	pos := model.Position{TsCode: "600000.SH", TotalQty: 200, TodayBought: 150}
	if got := weakExitQty(pos, 0.5); got != 0 {
		t.Errorf("可卖仅 50 股不足一手，应返回 0，实际 %d", int64(got))
	}
	pos.TodayBought = 0
	if got := weakExitQty(pos, 0.5); got != 100 {
		t.Errorf("可卖 200 股应减 100，实际 %d", int64(got))
	}
}

// TestEvalPriceRulesIntraday 盘中价格型规则：止损/移动止盈/止盈三条可判，
// 排名淘汰与大盘恶化**不参与**（盘中拿不到 TopN、指数 MA60 不能盘中伪造）。
func TestEvalPriceRulesIntraday(t *testing.T) {
	p := g1()
	// 止损：成本 10 元，现价 9.2 → 触发
	sig := EvalPriceRules("20260901", holding(model.FromFloat(10), 0, model.FromFloat(9.2), true, false), p)
	if sig == nil || sig.Rule != RuleStopLoss {
		t.Fatalf("盘中应触发止损: %+v", sig)
	}
	// 移动止盈：成本 10、高点 12、现价 11.4 → 触发
	sig = EvalPriceRules("20260901", holding(model.FromFloat(10), model.FromFloat(12), model.FromFloat(11.4), true, false), p)
	if sig == nil || sig.Rule != RuleTrailingStop {
		t.Fatalf("盘中应触发移动止盈: %+v", sig)
	}
	// 止盈：成本 10、现价 11.5（+15%）→ 触发
	sig = EvalPriceRules("20260901", holding(model.FromFloat(10), 0, model.FromFloat(11.5), true, false), p)
	if sig == nil || sig.Rule != RuleTakeProfit {
		t.Fatalf("盘中应触发止盈: %+v", sig)
	}
	// 关键：跌出 TopN（inTopN=false）但价格未触发任何价格型规则时，盘中**不**产生卖出信号
	if got := EvalPriceRules("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), false, true), p); got != nil {
		t.Errorf("排名淘汰/大盘恶化不应在盘中触发，实际得到 %+v", got)
	}
}

// TestEvalPriceRulesPriority 盘中优先级与 EvalSell 一致：止损 > 移动止盈 > 止盈。
func TestEvalPriceRulesPriority(t *testing.T) {
	p := g1()
	// 同时满足止损（9.0 < 9.2）与止盈（9.0 不满足）；构造同时满足移动止盈与止盈：
	// 成本 10、高点 12、现价 11.4 → 移动止盈线 11.40 破；止盈线 11.5 未到；以移动止盈为先。
	sig := EvalPriceRules("20260901", holding(model.FromFloat(10), model.FromFloat(12), model.FromFloat(11.4), true, false), p)
	if sig == nil || sig.Rule != RuleTrailingStop {
		t.Fatalf("移动止盈应优先: %+v", sig)
	}
}
