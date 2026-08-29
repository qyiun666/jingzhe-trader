package api

import (
	"fmt"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/model"
)

// RunPositions 持仓诊断
func (s *Service) RunPositions(date string) (*PortfolioJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	s.brk.UpdateMarketValue(todayBars)
	positions, _ := s.brk.QueryPositions()
	asset, _ := s.brk.QueryAsset()

	result := s.buildPortfolioJSON(positions, asset, todayBars)
	s.enrichPortfolioAnalysis(result, positions, todayBars)
	return result, nil
}

// enrichPortfolioAnalysis 用 analysis.AnalyzePortfolio 的完整分析覆盖简化指标
// (集中度/盈亏归因/风险指标/健康度评分)
func (s *Service) enrichPortfolioAnalysis(result *PortfolioJSON,
	positions map[string]*model.Position, todayBars map[string]*model.Bar) {

	totalAsset := result.TotalAsset
	if totalAsset <= 0 || len(positions) == 0 {
		return
	}

	stocks := make(map[string]*model.Stock, len(positions))
	for tsCode := range positions {
		if st, err := s.stockRepo.GetByCode(tsCode); err == nil && st != nil {
			stocks[tsCode] = st
		}
	}

	pa := analysis.AnalyzePortfolio(positions, todayBars, stocks, totalAsset, &dbHistoryAdapter{barRepo: s.barRepo})
	result.HealthScore = pa.HealthScore
	result.Concentration = map[string]float64{
		"top1_pct":   pa.Concentration.Top1Pct,
		"top3_pct":   pa.Concentration.Top3Pct,
		"top5_pct":   pa.Concentration.Top5Pct,
		"herfindahl": pa.Concentration.Herfindahl,
	}
	result.PnLSummary = map[string]interface{}{
		"total_pnl":   pa.PnLAttribution.TotalFloatingPnL,
		"win_count":   pa.PnLAttribution.WinCount,
		"loss_count":  pa.PnLAttribution.LossCount,
		"win_pct":     pa.PnLAttribution.WinPct,
		"best_stock":  pa.PnLAttribution.BestStock,
		"worst_stock": pa.PnLAttribution.WorstStock,
		"summary":     pa.Summary,
	}
	result.RiskMetrics = map[string]interface{}{
		"max_loss_pct":    pa.RiskMetrics.MaxSingleLossPct,
		"var95":           pa.RiskMetrics.VaR95,
		"beta":            pa.RiskMetrics.BetaToMarket,
		"suspended_count": pa.RiskMetrics.SuspendedCount,
	}
}

// buildPortfolioJSON 构建持仓诊断 JSON
func (s *Service) buildPortfolioJSON(
	positions map[string]*model.Position,
	asset *broker.AssetInfo,
	todayBars map[string]*model.Bar,
) *PortfolioJSON {
	totalAsset := asset.TotalAsset
	cash := asset.Cash
	marketValue := asset.MarketValue

	if totalAsset <= 0 {
		totalAsset = cash + marketValue
	}

	result := &PortfolioJSON{
		TotalAsset:  totalAsset,
		Cash:        cash,
		MarketValue: marketValue,
		HealthScore: 80,
		Concentration: map[string]float64{
			"top1_pct": 0,
			"top3_pct": 0,
			"top5_pct": 0,
		},
		SectorDist: []map[string]interface{}{},
		PnLSummary: map[string]interface{}{
			"total_pnl":  0.0,
			"win_count":  0,
			"loss_count": 0,
			"win_pct":    0.0,
		},
		RiskMetrics: map[string]interface{}{
			"max_loss_pct": 0.0,
			"var95":        0.0,
		},
		Holdings: []map[string]interface{}{},
	}

	if totalAsset <= 0 {
		return result
	}

	// 按市值排序的持仓列表
	type posInfo struct {
		tsCode      string
		pos         *model.Position
		marketValue float64
		weight      float64
	}
	var posList []posInfo
	var totalPnL float64
	var winCount, lossCount int
	var maxLossPct float64

	sectorMap := make(map[string]float64)

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		mv := pos.MarketValue
		if mv <= 0 && pos.MarketPrice > 0 {
			mv = float64(pos.TotalQty) * pos.MarketPrice
		}
		if mv <= 0 {
			continue
		}

		weight := mv / totalAsset
		posList = append(posList, posInfo{
			tsCode:      tsCode,
			pos:         pos,
			marketValue: mv,
			weight:      weight,
		})

		totalPnL += pos.FloatingPnL
		if pos.FloatingPnL > 0 {
			winCount++
		} else if pos.FloatingPnL < 0 {
			lossCount++
		}
		if pos.FloatingPnLPct < maxLossPct {
			maxLossPct = pos.FloatingPnLPct
		}

		// 板块分布
		sector := model.MarketFromCode(tsCode)
		sectorMap[sector] += weight

		// 持仓明细
		result.Holdings = append(result.Holdings, map[string]interface{}{
			"ts_code":      tsCode,
			"name":         s.stockName(tsCode),
			"total_qty":    pos.TotalQty,
			"cost_price":   pos.CostPrice,
			"market_price": pos.MarketPrice,
			"market_value": mv,
			"floating_pnl": pos.FloatingPnL,
			"pnl_pct":      pos.FloatingPnLPct,
			"weight_pct":   weight,
			"sector":       sector,
		})
	}

	// 按市值降序排序
	for i := 0; i < len(posList)-1; i++ {
		for j := i + 1; j < len(posList); j++ {
			if posList[j].marketValue > posList[i].marketValue {
				posList[i], posList[j] = posList[j], posList[i]
			}
		}
	}

	// 集中度
	if len(posList) >= 1 {
		result.Concentration["top1_pct"] = posList[0].weight
	}
	if len(posList) >= 3 {
		var sum float64
		for i := 0; i < 3; i++ {
			sum += posList[i].weight
		}
		result.Concentration["top3_pct"] = sum
	}
	if len(posList) >= 5 {
		var sum float64
		for i := 0; i < 5; i++ {
			sum += posList[i].weight
		}
		result.Concentration["top5_pct"] = sum
	}

	// 板块分布
	for sector, weight := range sectorMap {
		result.SectorDist = append(result.SectorDist, map[string]interface{}{
			"sector": sector,
			"weight": weight,
		})
	}

	// 盈亏摘要
	totalCount := winCount + lossCount
	winPct := 0.0
	if totalCount > 0 {
		winPct = float64(winCount) / float64(totalCount)
	}
	result.PnLSummary = map[string]interface{}{
		"total_pnl":  totalPnL,
		"win_count":  winCount,
		"loss_count": lossCount,
		"win_pct":    winPct,
	}

	// 风险指标
	result.RiskMetrics = map[string]interface{}{
		"max_loss_pct": maxLossPct,
		"var95":        0.0,
	}

	// 健康度评分 (简化版)
	healthScore := 80.0
	if top1Pct, ok := result.Concentration["top1_pct"]; ok && top1Pct > 0.5 {
		healthScore -= 20
	}
	if winPct < 0.5 && len(posList) > 0 {
		healthScore -= 10
	}
	if maxLossPct < -0.1 {
		healthScore -= 15
	}
	if healthScore < 0 {
		healthScore = 0
	}
	if healthScore > 100 {
		healthScore = 100
	}
	result.HealthScore = healthScore

	// 计算日收益率（对比昨日快照）
	var prevTotalAsset float64
	err := s.db.Get(&prevTotalAsset, "SELECT total_asset FROM account_snapshot ORDER BY trade_date DESC LIMIT 1")
	if err == nil && prevTotalAsset > 0 {
		result.DailyPnLPct = (totalAsset - prevTotalAsset) / prevTotalAsset
	}

	return result
}
