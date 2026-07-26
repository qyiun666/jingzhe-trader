package backtest

import (
	"math"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestCalculateMetrics_AllLossNoNaN 全亏损场景: 盈亏比应为 0 而非 NaN
func TestCalculateMetrics_AllLossNoNaN(t *testing.T) {
	snapshots := []model.AccountSnapshot{
		{TradeDate: "20260105", TotalAsset: 10000},
		{TradeDate: "20260106", TotalAsset: 9800},
		{TradeDate: "20260107", TotalAsset: 9600},
	}
	trades := []model.Trade{
		{TsCode: "A.SZ", Side: model.SideBuy, Price: 10, Qty: 100, TotalCost: 5, TradeDate: "20260105"},
		{TsCode: "A.SZ", Side: model.SideSell, Price: 9, Qty: 100, TotalCost: 5, TradeDate: "20260106"},
	}

	m := CalculateMetrics(snapshots, trades, nil)

	if math.IsNaN(m.ProfitLossRatio) || math.IsInf(m.ProfitLossRatio, 0) {
		t.Errorf("全亏损时盈亏比应为有限值, 实际 %v", m.ProfitLossRatio)
	}
	if m.ProfitLossRatio != 0 {
		t.Errorf("全亏损时盈亏比应为 0, 实际 %v", m.ProfitLossRatio)
	}
	if m.WinTrades != 0 || m.LossTrades != 1 {
		t.Errorf("交易统计错误: win=%d loss=%d", m.WinTrades, m.LossTrades)
	}
}

// TestCalculateMetrics_ProfitDeductsCost 交易级盈亏必须扣除手续费
func TestCalculateMetrics_ProfitDeductsCost(t *testing.T) {
	snapshots := []model.AccountSnapshot{
		{TradeDate: "20260105", TotalAsset: 10000},
		{TradeDate: "20260106", TotalAsset: 10005},
	}
	// 毛利 (10.1-10)*100 = 10 元, 手续费 5+10 = 15 元 → 净亏 5 元
	trades := []model.Trade{
		{TsCode: "A.SZ", Side: model.SideBuy, Price: 10, Qty: 100, TotalCost: 5, TradeDate: "20260105"},
		{TsCode: "A.SZ", Side: model.SideSell, Price: 10.1, Qty: 100, TotalCost: 10, TradeDate: "20260106"},
	}

	m := CalculateMetrics(snapshots, trades, nil)

	if m.WinTrades != 0 {
		t.Errorf("扣费后应为亏损交易, 实际 win=%d", m.WinTrades)
	}
	if m.WorstTrade > -4.9 || m.WorstTrade < -5.1 {
		t.Errorf("净亏应约为 -5 元, 实际 %.2f", m.WorstTrade)
	}
}
