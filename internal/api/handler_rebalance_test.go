package api

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
)

// 触发卖出但当日不可卖 (T+1 锁仓) 的持仓, 必须仍出现在日报持有列表
// 此前该分支既不入 sellList 也不入 holdList 就 continue,
// 用户当天买入的仓位会在日报里凭空消失
func TestBuildRebalanceKeepsTPlusOnePositionVisible(t *testing.T) {
	s := &Service{cfg: &config.Config{}}
	s.cfg.Risk.StopLossPct = 0.08
	s.cfg.Risk.TakeProfitPct = 0.15

	positions := map[string]*model.Position{
		"688049.SH": {TsCode: "688049.SH", TotalQty: 100, AvailableQty: 0, CostPrice: 45.78, MarketPrice: 45.78},
	}
	bars := map[string]*model.Bar{
		"688049.SH": {TsCode: "688049.SH", Close: 45.78, PreClose: 45.78},
	}
	asset := &broker.AssetInfo{TotalAsset: 11840.8, Cash: 5737.25, MarketValue: 6103.55}

	result := s.buildRebalanceJSON("20260831",
		[]model.Signal{{TsCode: "688049.SH", Direction: model.DirSell, TargetQty: 100, Reason: "布林带上轨突破"}},
		positions, asset, bars)

	if len(result.SellList) != 0 {
		t.Fatalf("T+1 不可卖不应出现在卖出列表: %+v", result.SellList)
	}
	if len(result.HoldList) != 1 {
		t.Fatalf("持仓数 1 时持有列表必须有 1 条, got %d", len(result.HoldList))
	}
	if !strings.Contains(result.HoldList[0].Suggestion, "T+1") {
		t.Fatalf("持有说明必须点明不可卖原因, got %q", result.HoldList[0].Suggestion)
	}
}

// 可卖量充足时仍按卖出建议输出 (确认上面的兜底没有吃掉正常路径)
func TestBuildRebalanceSellListUnchanged(t *testing.T) {
	s := &Service{cfg: &config.Config{}}
	s.cfg.Risk.StopLossPct = 0.08
	s.cfg.Risk.TakeProfitPct = 0.15

	positions := map[string]*model.Position{
		"600000.SH": {TsCode: "600000.SH", TotalQty: 200, AvailableQty: 200, CostPrice: 10, MarketPrice: 10},
	}
	bars := map[string]*model.Bar{"600000.SH": {TsCode: "600000.SH", Close: 8, PreClose: 10}}
	asset := &broker.AssetInfo{TotalAsset: 10000, Cash: 8000, MarketValue: 2000}

	result := s.buildRebalanceJSON("20260831", nil, positions, asset, bars)
	if len(result.SellList) != 1 || result.SellList[0].DeltaQty != -200 {
		t.Fatalf("止损应产出 200 股卖出建议: %+v", result.SellList)
	}
	if len(result.HoldList) != 0 {
		t.Fatalf("已列入卖出的持仓不应重复出现在持有列表: %+v", result.HoldList)
	}
}
