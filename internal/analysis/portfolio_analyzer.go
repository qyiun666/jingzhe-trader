package analysis

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/strategy"
)

// PortfolioAnalysis 持仓分析结果
type PortfolioAnalysis struct {
	// 集中度
	Concentration ConcentrationMetrics
	// 板块分布
	SectorDistribution map[string]SectorInfo
	// 盈亏归因
	PnLAttribution PnLAttribution
	// 风险指标
	RiskMetrics RiskMetrics
	// 健康度评分 (0-100)
	HealthScore float64
	// 总体评估
	Summary string
}

// ConcentrationMetrics 集中度指标
type ConcentrationMetrics struct {
	Top1Pct    float64 // 第一大持仓占比
	Top3Pct    float64 // 前三大持仓占比
	Top5Pct    float64 // 前五大持仓占比
	Herfindahl float64 // 赫芬达尔指数 (集中度, 越高越集中)
}

// SectorInfo 板块信息
type SectorInfo struct {
	Sector      string
	StockCount  int
	MarketValue float64
	WeightPct   float64 // 占组合比例
	PnL         float64 // 板块盈亏
	AvgPnLPct   float64 // 板块平均盈亏比例
}

// PnLAttribution 盈亏归因
type PnLAttribution struct {
	TotalFloatingPnL float64 // 总浮动盈亏
	TotalRealizedPnL float64 // 总实现盈亏 (如果有记录)
	BestStock        string  // 最赚的股票
	BestStockPnL     float64
	WorstStock       string  // 最亏的股票
	WorstStockPnL    float64
	WinCount         int     // 盈利股票数
	LossCount        int     // 亏损股票数
	WinPct           float64 // 胜率
}

// RiskMetrics 风险指标
type RiskMetrics struct {
	MaxSingleLossPct    float64 // 单票最大亏损比例
	MaxDrawdownFromCost float64 // 从成本价最大回撤
	BetaToMarket        float64 // 相对市场Beta (简化版)
	VaR95               float64 // 95% VaR (简化历史模拟法)
	// 停牌风险
	SuspendedCount int // 停牌股票数
}

// posWeight 持仓权重辅助结构
type posWeight struct {
	tsCode      string
	pos         *model.Position
	marketValue float64
	weight      float64 // 市值 / 总资产
}

// AnalyzePortfolio 分析持仓
func AnalyzePortfolio(
	positions map[string]*model.Position,
	bars map[string]*model.Bar,
	stocks map[string]*model.Stock,
	totalAsset float64,
	historyProvider strategy.HistoryProvider,
) *PortfolioAnalysis {
	pa := &PortfolioAnalysis{
		SectorDistribution: make(map[string]SectorInfo),
	}

	if totalAsset <= 0 {
		pa.Summary = "总资产为零,无法分析"
		return pa
	}

	// 提取一个交易日期用于历史数据查询
	var tradeDate string
	for _, bar := range bars {
		if bar != nil && bar.TradeDate != "" {
			tradeDate = bar.TradeDate
			break
		}
	}

	// 收集有效持仓并计算市值
	var posList []posWeight
	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		mv := pos.MarketValue
		if mv <= 0 {
			if bar := bars[tsCode]; bar != nil && bar.Close > 0 {
				mv = float64(pos.TotalQty) * bar.Close
			} else if pos.MarketPrice > 0 {
				mv = float64(pos.TotalQty) * pos.MarketPrice
			} else if pos.CostPrice > 0 {
				mv = float64(pos.TotalQty) * pos.CostPrice
			}
		}
		if mv <= 0 {
			continue
		}

		posList = append(posList, posWeight{
			tsCode:      tsCode,
			pos:         pos,
			marketValue: mv,
			weight:      mv / totalAsset,
		})
	}

	// 按市值降序排序
	sort.Slice(posList, func(i, j int) bool {
		return posList[i].marketValue > posList[j].marketValue
	})

	// 1. 计算集中度
	pa.calculateConcentration(posList)

	// 2. 计算板块分布
	pa.calculateSectorDistribution(posList, totalAsset)

	// 3. 计算盈亏归因
	pa.calculatePnLAttribution(posList)

	// 4. 计算风险指标
	pa.calculateRiskMetrics(posList, bars, tradeDate, historyProvider)

	// 5. 计算健康度评分
	pa.calculateHealthScore(posList)

	// 6. 生成总结
	pa.generateSummary(posList)

	return pa
}

// calculateConcentration 计算集中度指标
func (pa *PortfolioAnalysis) calculateConcentration(posList []posWeight) {
	var herfindahl float64
	for _, pw := range posList {
		herfindahl += pw.weight * pw.weight
	}
	pa.Concentration.Herfindahl = herfindahl

	if len(posList) >= 1 {
		pa.Concentration.Top1Pct = posList[0].weight
	}
	if len(posList) >= 3 {
		var sum float64
		for i := 0; i < 3; i++ {
			sum += posList[i].weight
		}
		pa.Concentration.Top3Pct = sum
	}
	if len(posList) >= 5 {
		var sum float64
		for i := 0; i < 5; i++ {
			sum += posList[i].weight
		}
		pa.Concentration.Top5Pct = sum
	}
}

// calculateSectorDistribution 计算板块分布
func (pa *PortfolioAnalysis) calculateSectorDistribution(posList []posWeight, totalAsset float64) {
	sectorPnLPcts := make(map[string][]float64)

	for _, pw := range posList {
		sector := model.MarketFromCode(pw.tsCode)
		info := pa.SectorDistribution[sector]
		info.Sector = sector
		info.StockCount++
		info.MarketValue += pw.marketValue
		info.PnL += pw.pos.FloatingPnL
		pa.SectorDistribution[sector] = info

		sectorPnLPcts[sector] = append(sectorPnLPcts[sector], pw.pos.FloatingPnLPct)
	}

	// 计算板块占比和平均盈亏比例
	for sector, info := range pa.SectorDistribution {
		if totalAsset > 0 {
			info.WeightPct = info.MarketValue / totalAsset
		}
		if pcts := sectorPnLPcts[sector]; len(pcts) > 0 {
			var sum float64
			for _, p := range pcts {
				sum += p
			}
			info.AvgPnLPct = sum / float64(len(pcts))
		}
		pa.SectorDistribution[sector] = info
	}
}

// calculatePnLAttribution 计算盈亏归因
func (pa *PortfolioAnalysis) calculatePnLAttribution(posList []posWeight) {
	var totalPnL float64
	var winCount, lossCount int
	bestStock, worstStock := "", ""
	bestPnL, worstPnL := 0.0, 0.0

	for _, pw := range posList {
		pnl := pw.pos.FloatingPnL
		totalPnL += pnl

		if pnl > 0 {
			winCount++
		} else if pnl < 0 {
			lossCount++
		}

		if pnl > bestPnL {
			bestPnL = pnl
			bestStock = pw.tsCode
		}
		if pnl < worstPnL {
			worstPnL = pnl
			worstStock = pw.tsCode
		}
	}

	pa.PnLAttribution.TotalFloatingPnL = totalPnL
	pa.PnLAttribution.BestStock = bestStock
	pa.PnLAttribution.BestStockPnL = bestPnL
	pa.PnLAttribution.WorstStock = worstStock
	pa.PnLAttribution.WorstStockPnL = worstPnL
	pa.PnLAttribution.WinCount = winCount
	pa.PnLAttribution.LossCount = lossCount

	totalCount := winCount + lossCount
	if totalCount > 0 {
		pa.PnLAttribution.WinPct = float64(winCount) / float64(totalCount)
	}
}

// calculateRiskMetrics 计算风险指标
func (pa *PortfolioAnalysis) calculateRiskMetrics(
	posList []posWeight,
	bars map[string]*model.Bar,
	tradeDate string,
	historyProvider strategy.HistoryProvider,
) {
	var maxLossPct float64 // 最负的浮动盈亏比例
	var maxDrawdown float64
	suspendedCount := 0

	for _, pw := range posList {
		// 单票最大亏损比例
		if pw.pos.FloatingPnLPct < maxLossPct {
			maxLossPct = pw.pos.FloatingPnLPct
		}

		// 从成本价的最大回撤 (简化版: 用当日最低价或收盘价)
		if pw.pos.CostPrice > 0 {
			currentPrice := pw.pos.MarketPrice
			if bar := bars[pw.tsCode]; bar != nil {
				if bar.Low > 0 {
					// 用当日最低价的回撤
					dd := (pw.pos.CostPrice - bar.Low) / pw.pos.CostPrice
					if dd > maxDrawdown {
						maxDrawdown = dd
					}
				} else if bar.Close > 0 {
					dd := (pw.pos.CostPrice - bar.Close) / pw.pos.CostPrice
					if dd > maxDrawdown {
						maxDrawdown = dd
					}
				}
			} else if currentPrice > 0 {
				dd := (pw.pos.CostPrice - currentPrice) / pw.pos.CostPrice
				if dd > maxDrawdown {
					maxDrawdown = dd
				}
			}
		}

		// 停牌检测: 当日无行情数据或成交量为0视为停牌
		if bar := bars[pw.tsCode]; bar == nil || (bar.Vol == 0 && bar.Close == 0) {
			suspendedCount++
		}
	}

	pa.RiskMetrics.MaxSingleLossPct = maxLossPct
	pa.RiskMetrics.MaxDrawdownFromCost = maxDrawdown
	pa.RiskMetrics.SuspendedCount = suspendedCount

	// 计算简化版 Beta (相对沪深300)
	pa.RiskMetrics.BetaToMarket = pa.calculateBeta(posList, tradeDate, historyProvider)

	// 计算 95% VaR (简化历史模拟法)
	pa.RiskMetrics.VaR95 = pa.calculateVaR(posList, tradeDate, historyProvider)
}

// calculateBeta 计算简化版 Beta
func (pa *PortfolioAnalysis) calculateBeta(
	posList []posWeight,
	tradeDate string,
	historyProvider strategy.HistoryProvider,
) float64 {
	if historyProvider == nil || tradeDate == "" {
		return 1.0
	}

	// 尝试获取沪深300指数历史数据
	marketBars, err := historyProvider.GetBars("000300.SH", tradeDate, 20)
	if err != nil || len(marketBars) < 10 {
		// 尝试上证指数
		marketBars, err = historyProvider.GetBars("000001.SH", tradeDate, 20)
		if err != nil || len(marketBars) < 10 {
			return 1.0
		}
	}

	marketReturns := make([]float64, len(marketBars))
	for i, b := range marketBars {
		marketReturns[i] = b.PctChg / 100.0 // 转换为小数
	}

	// 计算组合收益率序列 (按市值加权)
	var portfolioReturns []float64
	for i := range marketBars {
		var dailyReturn float64
		var totalWeight float64

		for _, pw := range posList {
			stockBars, err := historyProvider.GetBars(pw.tsCode, tradeDate, 20)
			if err != nil || len(stockBars) != len(marketBars) {
				continue
			}
			if i < len(stockBars) {
				dailyReturn += pw.weight * (stockBars[i].PctChg / 100.0)
				totalWeight += pw.weight
			}
		}

		if totalWeight > 0 {
			portfolioReturns = append(portfolioReturns, dailyReturn/totalWeight)
		} else {
			portfolioReturns = append(portfolioReturns, 0)
		}
	}

	if len(portfolioReturns) < 5 {
		return 1.0
	}

	// 计算协方差和方差
	marketMean := mean(marketReturns)
	portMean := mean(portfolioReturns)

	var cov, marketVar float64
	minLen := len(portfolioReturns)
	if len(marketReturns) < minLen {
		minLen = len(marketReturns)
	}

	for i := 0; i < minLen; i++ {
		cov += (portfolioReturns[i] - portMean) * (marketReturns[i] - marketMean)
		marketVar += (marketReturns[i] - marketMean) * (marketReturns[i] - marketMean)
	}

	if marketVar == 0 {
		return 1.0
	}

	beta := cov / marketVar
	// 限制在合理范围内
	if beta < -2 {
		beta = -2
	} else if beta > 3 {
		beta = 3
	}
	return beta
}

// calculateVaR 计算 95% VaR (简化历史模拟法)
func (pa *PortfolioAnalysis) calculateVaR(
	posList []posWeight,
	tradeDate string,
	historyProvider strategy.HistoryProvider,
) float64 {
	if historyProvider == nil || tradeDate == "" || len(posList) == 0 {
		return 0
	}

	// 收集每只股票最近20日的日收益率, 按持仓权重加权得到组合日收益率序列
	var portfolioReturns []float64

	for day := 0; day < 20; day++ {
		var dailyReturn float64
		var hasData bool

		for _, pw := range posList {
			bars, err := historyProvider.GetBars(pw.tsCode, tradeDate, 20)
			if err != nil || len(bars) == 0 {
				continue
			}
			idx := len(bars) - 20 + day
			if idx < 0 {
				idx = 0
			}
			if idx < len(bars) {
				dailyReturn += pw.weight * (bars[idx].PctChg / 100.0)
				hasData = true
			}
		}

		if hasData {
			portfolioReturns = append(portfolioReturns, dailyReturn)
		}
	}

	if len(portfolioReturns) == 0 {
		return 0
	}

	// 取5%分位数 (最坏的5%情况)
	sort.Float64s(portfolioReturns)
	idx := int(math.Ceil(0.05*float64(len(portfolioReturns)))) - 1
	if idx < 0 {
		idx = 0
	}
	var95 := -portfolioReturns[idx] // 转换为正数表示风险金额比例
	if var95 < 0 {
		var95 = 0
	}
	return var95
}

// calculateHealthScore 计算健康度评分
func (pa *PortfolioAnalysis) calculateHealthScore(posList []posWeight) {
	// 1. 集中度评分 (30分): Herfindahl 越低越好
	concentrationScore := 30.0
	h := pa.Concentration.Herfindahl
	if h >= 0.3 {
		concentrationScore = 0
	} else if h >= 0.2 {
		concentrationScore = 10
	} else if h >= 0.1 {
		concentrationScore = 20
	} else {
		concentrationScore = 30
	}

	// 2. 板块分散度评分 (20分): 板块越多越分散
	sectorCount := len(pa.SectorDistribution)
	sectorScore := 0.0
	if sectorCount >= 5 {
		sectorScore = 20
	} else if sectorCount >= 4 {
		sectorScore = 15
	} else if sectorCount >= 3 {
		sectorScore = 10
	} else if sectorCount >= 2 {
		sectorScore = 5
	}

	// 3. 盈亏状况评分 (30分): 胜率越高越好
	pnlScore := pa.PnLAttribution.WinPct * 30.0

	// 4. 风险水平评分 (20分): 最大亏损越小越好
	riskScore := 20.0
	maxLoss := pa.RiskMetrics.MaxSingleLossPct
	if maxLoss <= -0.2 {
		riskScore = 0
	} else if maxLoss <= -0.1 {
		riskScore = 10
	} else if maxLoss < 0 {
		riskScore = 15
	} else {
		riskScore = 20
	}

	pa.HealthScore = concentrationScore + sectorScore + pnlScore + riskScore
	if pa.HealthScore > 100 {
		pa.HealthScore = 100
	}
	if pa.HealthScore < 0 {
		pa.HealthScore = 0
	}
}

// generateSummary 生成总体评估
func (pa *PortfolioAnalysis) generateSummary(posList []posWeight) {
	var parts []string

	if pa.HealthScore >= 80 {
		parts = append(parts, "持仓健康状况良好")
	} else if pa.HealthScore >= 60 {
		parts = append(parts, "持仓健康状况一般")
	} else {
		parts = append(parts, "持仓健康状况较差,建议调整")
	}

	parts = append(parts, fmt.Sprintf("持仓数量:%d", len(posList)))
	parts = append(parts, fmt.Sprintf("集中度(HHI):%.2f", pa.Concentration.Herfindahl))

	if pa.PnLAttribution.TotalFloatingPnL > 0 {
		parts = append(parts, fmt.Sprintf("总体浮盈:%.2f", pa.PnLAttribution.TotalFloatingPnL))
	} else {
		parts = append(parts, fmt.Sprintf("总体浮亏:%.2f", pa.PnLAttribution.TotalFloatingPnL))
	}

	if pa.RiskMetrics.SuspendedCount > 0 {
		parts = append(parts, fmt.Sprintf("注意:%d只股票停牌", pa.RiskMetrics.SuspendedCount))
	}

	pa.Summary = strings.Join(parts, "; ")
}

// mean 计算平均值
func mean(data []float64) float64 {
	if len(data) == 0 {
		return 0
	}
	var sum float64
	for _, v := range data {
		sum += v
	}
	return sum / float64(len(data))
}
