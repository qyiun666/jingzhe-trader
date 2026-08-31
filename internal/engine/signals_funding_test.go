// applyFunding 批次资金核算的行为测试
package engine

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

func testCost() *market.CostModel {
	return market.NewCostModel(config.CostConfig{
		CommissionRate: 0.00025, MinCommission: 5, StampTaxRate: 0.0005, TransferFeeRate: 0.00001,
	})
}

func barOf(close float64) map[string]*model.Bar {
	return map[string]*model.Bar{"X.SH": {TsCode: "X.SH", Close: close}}
}

// TestApplyFundingTruncatesToAffordable 现金买不满请求量时按整手缩量, 并把缩量原因留在信号上
func TestApplyFundingTruncatesToAffordable(t *testing.T) {
	in := SignalInput{Cash: 2500, Cost: testCost(), Bars: barOf(20)}
	sig := model.Signal{TsCode: "X.SH", Direction: model.DirBuy, TargetQty: 1000, Reason: "金叉"}
	got, rej := applyFunding(in, []model.Signal{sig})
	if len(rej) != 0 || len(got) != 1 {
		t.Fatalf("应缩量通过, 实际 got=%d rej=%d", len(got), len(rej))
	}
	if got[0].TargetQty != 100 {
		t.Fatalf("2500 元只够 1 手 20 元股, 实际 %d", got[0].TargetQty)
	}
	if !strings.Contains(got[0].Reason, "资金调整") {
		t.Errorf("缩量必须留痕, 实际 %q", got[0].Reason)
	}
}

// TestApplyFundingCreditsBatchSellProceeds 换仓场景: 本批次卖出的净回款必须先计入可用资金
func TestApplyFundingCreditsBatchSellProceeds(t *testing.T) {
	// 现金仅 100, 卖 1000 股 @20 净回款约 19985, 之后买 900 股 @20 (含费 18005) 才买得起
	in := SignalInput{Cash: 100, Cost: testCost(), Bars: map[string]*model.Bar{
		"X.SH": {TsCode: "X.SH", Close: 20}, "Y.SZ": {TsCode: "Y.SZ", Close: 20},
	}}
	sell := model.Signal{TsCode: "X.SH", Direction: model.DirSell, TargetQty: 1000}
	buy := model.Signal{TsCode: "Y.SZ", Direction: model.DirBuy, TargetQty: 900}
	got, rej := applyFunding(in, []model.Signal{sell, buy})
	if len(rej) != 0 {
		t.Fatalf("卖出回款应支持该买入, 实际拒绝 %+v", rej)
	}
	if len(got) != 2 {
		t.Fatalf("应保留卖出+买入两条, 实际 %d", len(got))
	}
}

// TestApplyFundingKeepsStrongestAndRejectsUnaffordable 资金只够一笔时保留强度高的, 其余明确拒因
func TestApplyFundingKeepsStrongestAndRejectsUnaffordable(t *testing.T) {
	in := SignalInput{Cash: 2100, Cost: testCost(),
		Bars: map[string]*model.Bar{"X.SH": {TsCode: "X.SH", Close: 20}, "Y.SZ": {TsCode: "Y.SZ", Close: 20}}}
	weak := model.Signal{TsCode: "Y.SZ", Direction: model.DirBuy, TargetQty: 200, Strength: 0.2}
	strong := model.Signal{TsCode: "X.SH", Direction: model.DirBuy, TargetQty: 100, Strength: 0.9}

	got, rej := applyFunding(in, []model.Signal{weak, strong})
	if len(got) != 1 || got[0].TsCode != "X.SH" {
		t.Fatalf("应保留强度更高的 X.SH, 实际 %+v", got)
	}
	if len(rej) != 1 || rej[0].Rule != "insufficient_cash" {
		t.Fatalf("Y.SZ 应因资金不足被拒, 实际 %+v", rej)
	}
	if !strings.Contains(rej[0].Reason, "可用资金不足") {
		t.Errorf("拒绝原因需可读, 实际 %q", rej[0].Reason)
	}
}

// TestApplyFundingSkippedWithoutCashSnapshot 没有资金快照时不得假装约束
func TestApplyFundingSkippedWithoutCashSnapshot(t *testing.T) {
	in := SignalInput{Cash: 0, Cost: testCost(), Bars: barOf(20)}
	sig := model.Signal{TsCode: "X.SH", Direction: model.DirBuy, TargetQty: 100000}
	got, rej := applyFunding(in, []model.Signal{sig})
	if len(got) != 1 || len(rej) != 0 || got[0].TargetQty != 100000 {
		t.Fatalf("无资金快照应原样放行, 实际 got=%d rej=%d", len(got), len(rej))
	}
}

// TestApplyFundingWithoutPricePassesThrough 无价格可核算时保持原量, 不凭空裁剪
func TestApplyFundingWithoutPricePassesThrough(t *testing.T) {
	in := SignalInput{Cash: 100, Cost: testCost(), Bars: map[string]*model.Bar{}}
	sig := model.Signal{TsCode: "X.SH", Direction: model.DirBuy, TargetQty: 500}
	got, rej := applyFunding(in, []model.Signal{sig})
	if len(got) != 1 || got[0].TargetQty != 500 || len(rej) != 0 {
		t.Fatalf("无价格时应原样放行, 实际 %+v rej=%d", got, len(rej))
	}
}
