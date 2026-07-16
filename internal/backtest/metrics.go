package backtest

import (
	"math"

	"jingzhe-trader/internal/model"
)

// Metrics 绩效指标
type Metrics struct {
	TotalReturn      float64 // 总收益率
	AnnualReturn     float64 // 年化收益率
	SharpeRatio      float64 // 夏普比率
	MaxDrawdown      float64 // 最大回撤
	MaxDrawdownStart string  // 最大回撤起始日
	MaxDrawdownEnd   string  // 最大回撤结束日
	WinRate          float64 // 胜率
	ProfitLossRatio  float64 // 盈亏比
	TotalTrades      int     // 总交易次数
	WinTrades        int     // 盈利交易次数
	LossTrades       int     // 亏损交易次数
	AvgWin           float64 // 平均盈利
	AvgLoss          float64 // 平均亏损
	BestTrade        float64 // 单笔最大盈利
	WorstTrade       float64 // 单笔最大亏损
	TradingDays      int     // 交易天数
	Alpha            float64 // Alpha
	Beta             float64 // Beta
}

// CalculateMetrics 计算绩效指标
func CalculateMetrics(snapshots []model.AccountSnapshot, trades []model.Trade, benchmarkReturns []float64) Metrics {
	m := Metrics{}
	if len(snapshots) == 0 {
		return m
	}

	m.TradingDays = len(snapshots)
	initialAsset := snapshots[0].TotalAsset
	finalAsset := snapshots[len(snapshots)-1].TotalAsset

	// 总收益率
	m.TotalReturn = (finalAsset - initialAsset) / initialAsset

	// 年化收益率 (假设252个交易日)
	years := float64(m.TradingDays) / 252.0
	if years > 0 {
		m.AnnualReturn = math.Pow(finalAsset/initialAsset, 1/years) - 1
	}

	// 日收益率序列
	dailyReturns := make([]float64, len(snapshots)-1)
	for i := 1; i < len(snapshots); i++ {
		if snapshots[i-1].TotalAsset > 0 {
			dailyReturns[i-1] = (snapshots[i].TotalAsset - snapshots[i-1].TotalAsset) / snapshots[i-1].TotalAsset
		}
	}

	// 夏普比率 (无风险利率默认2%)
	if len(dailyReturns) > 1 {
		avgReturn := mean(dailyReturns)
		stdReturn := stdDev(dailyReturns)
		if stdReturn > 0 {
			dailyRiskFree := 0.02 / 252.0
			m.SharpeRatio = (avgReturn - dailyRiskFree) / stdReturn * math.Sqrt(252)
		}
	}

	// 最大回撤
	peak := snapshots[0].TotalAsset
	maxDD := 0.0
	ddStart := snapshots[0].TradeDate
	for _, snap := range snapshots {
		if snap.TotalAsset > peak {
			peak = snap.TotalAsset
			ddStart = snap.TradeDate
		}
		dd := (peak - snap.TotalAsset) / peak
		if dd > maxDD {
			maxDD = dd
			m.MaxDrawdownEnd = snap.TradeDate
			m.MaxDrawdownStart = ddStart
		}
	}
	m.MaxDrawdown = maxDD

	// 交易统计 (按卖出成交计算每笔盈亏)
	// 需要配对买卖交易来计算盈亏, 这里简化处理: 按卖出时的盈亏
	// 完整实现需要跟踪每笔买入的成本
	type tradePair struct {
		buyAmount  float64
		buyQty     int
		sellAmount float64
		sellQty    int
	}
	pairs := make(map[string]*tradePair) // tsCode -> tradePair
	var profits []float64

	for _, t := range trades {
		pair, ok := pairs[t.TsCode]
		if !ok {
			pair = &tradePair{}
			pairs[t.TsCode] = pair
		}
		if t.Side == model.SideBuy {
			pair.buyAmount += t.Price * float64(t.Qty)
			pair.buyQty += t.Qty
		} else {
			pair.sellAmount += t.Price * float64(t.Qty)
			pair.sellQty += t.Qty
			// 卖出时计算这笔的盈亏
			if pair.buyQty > 0 {
				avgBuyPrice := pair.buyAmount / float64(pair.buyQty)
				profit := (t.Price - avgBuyPrice) * float64(t.Qty)
				profits = append(profits, profit)
				// 更新买入成本 (扣除已卖出的部分)
				pair.buyAmount -= avgBuyPrice * float64(t.Qty)
				pair.buyQty -= t.Qty
			}
		}
	}

	m.TotalTrades = len(profits)
	if m.TotalTrades > 0 {
		totalWin := 0.0
		totalLoss := 0.0
		m.BestTrade = profits[0]
		m.WorstTrade = profits[0]
		for _, p := range profits {
			if p > 0 {
				m.WinTrades++
				totalWin += p
			} else {
				m.LossTrades++
				totalLoss += p
			}
			if p > m.BestTrade {
				m.BestTrade = p
			}
			if p < m.WorstTrade {
				m.WorstTrade = p
			}
		}
		m.WinRate = float64(m.WinTrades) / float64(m.TotalTrades)
		if m.LossTrades > 0 {
			avgLoss := totalLoss / float64(m.LossTrades)
			m.AvgLoss = avgLoss
			if avgLoss != 0 {
				if m.WinTrades > 0 {
					m.AvgWin = totalWin / float64(m.WinTrades)
				}
				m.ProfitLossRatio = (totalWin / float64(m.WinTrades)) / (-avgLoss)
			}
		}
	}

	// Alpha & Beta (与基准对比)
	if len(benchmarkReturns) > 0 && len(dailyReturns) > 0 {
		minLen := len(dailyReturns)
		if len(benchmarkReturns) < minLen {
			minLen = len(benchmarkReturns)
		}
		if minLen > 1 {
			portReturns := dailyReturns[:minLen]
			benchReturns := benchmarkReturns[:minLen]
			m.Beta = calculateBeta(portReturns, benchReturns)
			m.Alpha = calculateAlpha(portReturns, benchReturns, m.Beta)
		}
	}

	return m
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}

func stdDev(values []float64) float64 {
	if len(values) < 2 {
		return 0
	}
	avg := mean(values)
	sumSq := 0.0
	for _, v := range values {
		diff := v - avg
		sumSq += diff * diff
	}
	return math.Sqrt(sumSq / float64(len(values)-1))
}

func calculateBeta(portReturns, benchReturns []float64) float64 {
	if len(portReturns) != len(benchReturns) || len(portReturns) < 2 {
		return 0
	}
	portAvg := mean(portReturns)
	benchAvg := mean(benchReturns)
	cov := 0.0
	benchVar := 0.0
	for i := range portReturns {
		cov += (portReturns[i] - portAvg) * (benchReturns[i] - benchAvg)
		benchVar += (benchReturns[i] - benchAvg) * (benchReturns[i] - benchAvg)
	}
	cov /= float64(len(portReturns) - 1)
	benchVar /= float64(len(portReturns) - 1)
	if benchVar > 0 {
		return cov / benchVar
	}
	return 0
}

func calculateAlpha(portReturns, benchReturns []float64, beta float64) float64 {
	// Alpha = Rp - (Rf + Beta * (Rb - Rf))
	// 简化: Rf = 0.02/252
	rf := 0.02 / 252.0
	rp := mean(portReturns)
	rb := mean(benchReturns)
	return (rp - rf - beta*(rb-rf)) * 252 // 年化
}
