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

// TestCalculateMetrics_NewRatios 新增指标: 年化波动率/索提诺/卡玛/月胜率/总交易成本
func TestCalculateMetrics_NewRatios(t *testing.T) {
	// 跨3个月: 1月持平(月末=首日), 2月下跌, 3月上涨 → 月胜率 1/3
	snapshots := []model.AccountSnapshot{
		{TradeDate: "20260130", TotalAsset: 10000},
		{TradeDate: "20260202", TotalAsset: 10100},
		{TradeDate: "20260227", TotalAsset: 9900},
		{TradeDate: "20260302", TotalAsset: 10200},
	}
	trades := []model.Trade{
		{TsCode: "A.SZ", Side: model.SideBuy, Price: 10, Qty: 100, Commission: 5, TransferFee: 1, TotalCost: 6, TradeDate: "20260130"},
		{TsCode: "A.SZ", Side: model.SideSell, Price: 10.1, Qty: 100, Commission: 5, StampTax: 1, TransferFee: 1, TotalCost: 7, TradeDate: "20260202"},
	}

	m := CalculateMetrics(snapshots, trades, nil)

	// 日收益率: [0.01, -200/10100, 300/9900]
	dailyReturns := []float64{0.01, -200.0 / 10100.0, 300.0 / 9900.0}

	// 年化波动率 = 日收益标准差(样本) * sqrt(252)
	expVol := stdDev(dailyReturns) * math.Sqrt(252)
	if math.Abs(m.AnnualVolatility-expVol) > 1e-9 {
		t.Errorf("年化波动率错误: 期望 %v, 实际 %v", expVol, m.AnnualVolatility)
	}

	// 索提诺比率: 下行波动相对无风险利率 (与夏普一致 rf=2%)
	dailyRiskFree := 0.02 / 252.0
	downsideSq := 0.0
	for _, r := range dailyReturns {
		if d := r - dailyRiskFree; d < 0 {
			downsideSq += d * d
		}
	}
	downsideDev := math.Sqrt(downsideSq / float64(len(dailyReturns)))
	expSortino := (mean(dailyReturns) - dailyRiskFree) / downsideDev * math.Sqrt(252)
	if math.Abs(m.SortinoRatio-expSortino) > 1e-9 {
		t.Errorf("索提诺比率错误: 期望 %v, 实际 %v", expSortino, m.SortinoRatio)
	}

	// 卡玛比率 = 年化收益/最大回撤 (最大回撤: 10100→9900)
	expDD := 200.0 / 10100.0
	if math.Abs(m.MaxDrawdown-expDD) > 1e-9 {
		t.Errorf("最大回撤错误: 期望 %v, 实际 %v", expDD, m.MaxDrawdown)
	}
	expCalmar := m.AnnualReturn / expDD
	if math.Abs(m.CalmarRatio-expCalmar) > 1e-9 {
		t.Errorf("卡玛比率错误: 期望 %v, 实际 %v", expCalmar, m.CalmarRatio)
	}

	// 月胜率: 1月 10000→10000 持平, 2月 10000→9900 亏, 3月 9900→10200 盈 → 1/3
	expMonthlyWin := 1.0 / 3.0
	if math.Abs(m.MonthlyWinRate-expMonthlyWin) > 1e-9 {
		t.Errorf("月胜率错误: 期望 %v, 实际 %v", expMonthlyWin, m.MonthlyWinRate)
	}

	// 总交易成本 = (5+0+1) + (5+1+1) = 13
	if math.Abs(m.TotalTradeCost-13) > 1e-9 {
		t.Errorf("总交易成本错误: 期望 13, 实际 %v", m.TotalTradeCost)
	}
}

// TestCalculateMetrics_TotalCostFallback 仅有 TotalCost 字段时总交易成本应回退兼容
func TestCalculateMetrics_TotalCostFallback(t *testing.T) {
	snapshots := []model.AccountSnapshot{
		{TradeDate: "20260105", TotalAsset: 10000},
		{TradeDate: "20260106", TotalAsset: 10000},
	}
	trades := []model.Trade{
		{TsCode: "A.SZ", Side: model.SideBuy, Price: 10, Qty: 100, TotalCost: 5, TradeDate: "20260105"},
		{TsCode: "A.SZ", Side: model.SideSell, Price: 10, Qty: 100, TotalCost: 5, TradeDate: "20260106"},
	}

	m := CalculateMetrics(snapshots, trades, nil)
	if math.Abs(m.TotalTradeCost-10) > 1e-9 {
		t.Errorf("回退后总交易成本应为 10, 实际 %v", m.TotalTradeCost)
	}
}

// TestCalculateMetrics_SingleSnapshot 单快照/空数据时新增指标应为 0 且无 NaN
func TestCalculateMetrics_SingleSnapshot(t *testing.T) {
	m := CalculateMetrics([]model.AccountSnapshot{{TradeDate: "20260105", TotalAsset: 10000}}, nil, nil)
	for name, v := range map[string]float64{
		"CalmarRatio": m.CalmarRatio, "SortinoRatio": m.SortinoRatio,
		"AnnualVolatility": m.AnnualVolatility, "MonthlyWinRate": m.MonthlyWinRate,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v != 0 {
			t.Errorf("%s 应为 0, 实际 %v", name, v)
		}
	}
	// 单月且月末>首日: 月胜率 100%
	m2 := CalculateMetrics([]model.AccountSnapshot{
		{TradeDate: "20260105", TotalAsset: 10000},
		{TradeDate: "20260106", TotalAsset: 10100},
	}, nil, nil)
	if m2.MonthlyWinRate != 1 {
		t.Errorf("单月上涨时月胜率应为 1, 实际 %v", m2.MonthlyWinRate)
	}
}
