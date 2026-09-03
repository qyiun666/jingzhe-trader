package signal

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
)

// g1 G1 生效参数（止损 8% / 移动止盈 5% / 止盈 15%）。
func g1() risk.RiskParams {
	return risk.Resolve(risk.DefaultBase(model.FromFloat(100000)), model.GearG1, false, risk.NoPace{})
}

// holding 构造持仓上下文。
func holding(cost, high, last model.Fen, inTopN, inWatch, marketBad bool) HoldingCtx {
	return HoldingCtx{
		Pos: model.Position{
			TsCode: "sh600001", TotalQty: 1000, AvailableQty: 1000,
			CostPrice: cost, HighPrice: high,
		},
		LastClose: last,
		InTopN:    inTopN, InWatch: inWatch, MarketBad: marketBad,
	}
}

// TestSellStopLoss 规则 1 止损：收盘跌破止损线触发；未跌破不触发。
func TestSellStopLoss(t *testing.T) {
	p := g1()
	// 成本 10 元，止损线 = 9.20 元
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(9.2), true, false, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleStopLoss {
		t.Fatalf("应触发止损: %+v", sig)
	}
	if sig.Direction != model.DirSell {
		t.Errorf("方向=%s, 期望 sell", sig.Direction)
	}
	if !strings.Contains(sig.Reason, "止损") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(9.21), true, false, false), p, 0, 0) != nil {
		t.Errorf("9.21 元未破止损线，不应触发")
	}
}

// TestSellTrailingStop 规则 2 移动止盈：自高点回撤超线且已有盈利时触发。
func TestSellTrailingStop(t *testing.T) {
	p := g1()
	// 成本 10 元，高点 12 元，回撤线 = 12×0.95 = 11.40
	sig := EvalSell("20260901", holding(model.FromFloat(10), model.FromFloat(12), model.FromFloat(11.4), true, false, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleTrailingStop {
		t.Fatalf("应触发移动止盈: %+v", sig)
	}
	if !strings.Contains(sig.Reason, "回撤") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	// 11.41 未破回撤线 → 不触发
	if EvalSell("20260901", holding(model.FromFloat(10), model.FromFloat(12), model.FromFloat(11.41), true, false, false), p, 0, 0) != nil {
		t.Errorf("未破回撤线不应触发")
	}
	// 高点低于成本（未盈利过）→ 不做移动止盈，回落走止损规则
	// （成本 12，止损线 11.04，现价 10.40 已破线 → 期望 stop_loss）
	if sig := EvalSell("20260901", holding(model.FromFloat(12), model.FromFloat(11), model.FromFloat(10.4), true, false, false), p, 0, 0); sig == nil || sig.Rule != RuleStopLoss {
		t.Errorf("高点未过成本不应触发移动止盈，应回落止损: %+v", sig)
	}
	// 高点高于成本但未破回撤线 → 不触发任何规则
	if sig := EvalSell("20260901", holding(model.FromFloat(12), model.FromFloat(13), model.FromFloat(12.5), true, false, false), p, 0, 0); sig != nil {
		t.Errorf("盈利未回撤不应触发: %+v", sig)
	}
}

// TestSellTakeProfit 规则 3 止盈：收盘达到止盈线触发。
func TestSellTakeProfit(t *testing.T) {
	p := g1()
	// 成本 10 元，止盈线 = 11.50 元
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(11.5), true, false, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleTakeProfit {
		t.Fatalf("应触发止盈: %+v", sig)
	}
	if !strings.Contains(sig.Reason, "止盈") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(11.49), true, false, false), p, 0, 0) != nil {
		t.Errorf("11.49 元未到止盈线不应触发")
	}
}

// TestSellRankOut 规则 4 排名淘汰：不在候选池且不在观察名单触发。
func TestSellRankOut(t *testing.T) {
	p := g1()
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), false, false, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleRankOut {
		t.Fatalf("应触发排名淘汰: %+v", sig)
	}
	// 在候选池 → 不触发
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false, false), p, 0, 0) != nil {
		t.Errorf("在候选池不应触发排名淘汰")
	}
	// 在观察名单 → 不触发（降级保护）
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), false, true, false), p, 0, 0) != nil {
		t.Errorf("在观察名单不应触发排名淘汰")
	}
}

// TestSellMarketBad 规则 5 大盘恶化：指数收盘跌破 MA20 触发。
func TestSellMarketBad(t *testing.T) {
	p := g1()
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false, false), p,
		model.FromFloat(3000), 3100.0)
	if sig == nil || sig.Rule != RuleMarketBad {
		t.Fatalf("应触发大盘恶化: %+v", sig)
	}
	if !strings.Contains(sig.Reason, "MA20") {
		t.Errorf("理由不可解释: %q", sig.Reason)
	}
	// 指数在 MA20 之上 → 不触发
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false, false), p,
		model.FromFloat(3200), 3100.0) != nil {
		t.Errorf("指数未破 MA20 不应触发")
	}
	// 指数数据缺失（close=0）→ 不判定
	if EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(10), true, false, false), p, 0, 0) != nil {
		t.Errorf("指数缺失不应触发大盘恶化")
	}
}

// TestSellPriority 优先级：同一持仓同时满足多条时只出首条（止损优先）。
func TestSellPriority(t *testing.T) {
	p := g1()
	// 同时满足止损（跌破 9.2）与排名淘汰
	sig := EvalSell("20260901", holding(model.FromFloat(10), 0, model.FromFloat(9), false, false, false), p, 0, 0)
	if sig == nil || sig.Rule != RuleStopLoss {
		t.Fatalf("止损应优先: %+v", sig)
	}
}

// TestEvalBuyTrend 买入规则：MA5>MA20 且量比达标且置信度足够。
func TestEvalBuyTrend(t *testing.T) {
	p := g1()
	c := BuyCandidate{Result: model.ScreenResult{
		TradeDate: "20260901", TsCode: "sh600002", Rank: 1, Score: 80, Close: model.FromFloat(20),
	}, Name: "乙"}

	// 构造上升趋势 + 放量序列：30 根，末段抬升
	closes := make([]float64, 0, 30)
	vols := make([]float64, 0, 30)
	for i := 0; i < 30; i++ {
		closes = append(closes, 10+float64(i)*0.2)
		vols = append(vols, 100)
	}
	vols[29] = 150 // 量比 1.5
	sig := EvalBuy(c, BarSeries{Closes: closes, Vols: vols}, p)
	if sig == nil {
		t.Fatalf("趋势+放量应触发买入信号")
	}
	if sig.Rule != BuyRuleName || sig.Direction != model.DirBuy {
		t.Errorf("信号字段错误: %+v", sig)
	}
	if sig.Confidence < p.MinConfidence {
		t.Errorf("置信度 %.2f 低于档位下限 %.2f", sig.Confidence, p.MinConfidence)
	}
	// 缩量 → 不触发
	vols[29] = 100
	if EvalBuy(c, BarSeries{Closes: closes, Vols: vols}, p) != nil {
		t.Errorf("缩量不应触发")
	}
	// 数据不足 → 不触发（不 panic）
	if EvalBuy(c, BarSeries{Closes: closes[:10], Vols: vols[:10]}, p) != nil {
		t.Errorf("数据不足不应触发")
	}
}

// TestPassThroughConfirmer 默认终审直接放行（Batch 4 替换为 LLM 实现）。
func TestPassThroughConfirmer(t *testing.T) {
	ok, err := PassThroughConfirmer{}.Confirm(nil, BuyCandidate{})
	if err != nil || !ok {
		t.Errorf("默认终审应无条件放行: ok=%v err=%v", ok, err)
	}
}
