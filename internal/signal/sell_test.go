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
