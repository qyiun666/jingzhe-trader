package api

import (
	"fmt"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// RunDaily 生成每日操盘报告 (JSON)
func (s *Service) RunDaily(date string, strategyName string) (*DailyReportJSON, error) {
	// 1. 获取当日全市场行情
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取当日行情失败: %w", err)
	}
	if len(allBars) == 0 {
		return nil, fmt.Errorf("当日 %s 无行情数据", date)
	}

	// 2. 构建当日行情 map
	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	// 3. 获取上一交易日行情
	prevBars := s.getPrevBars(date)
	analysis.SetPrevBars(prevBars)

	// 4. 获取持仓和资产
	positions := s.getPositions()
	asset := s.getAsset()
	s.brk.UpdateMarketValue(todayBars)
	positions, _ = s.brk.QueryPositions()
	asset, _ = s.brk.QueryAsset()

	// 5. 市场快照
	marketSnapshot := s.buildMarketSnapshot(date, allBars, prevBars)

	// 6. 策略信号 (日报场景策略失败降级为空信号并在日志中体现, 不阻断日报)
	signals, sigErr := s.runStrategy(date, strategyName, todayBars, positions, asset)
	if sigErr != nil {
		logger.L().Errorf("[%s] 日报策略信号生成失败: %v", date, sigErr)
	}

	// 7. 持仓诊断
	portfolioJSON := s.buildPortfolioJSON(positions, asset, todayBars)

	// 8. 调仓计划
	rebalanceJSON := s.buildRebalanceJSON(date, signals, positions, asset, todayBars)

	// 9. 策略建议
	strategyJSON := s.buildStrategyJSON(date, signals, todayBars, portfolioJSON)

	// 10. 新闻摘要
	newsJSON := s.buildNewsJSON()

	// 11. 操作清单
	actionItems := s.buildActionItems(signals, portfolioJSON, marketSnapshot)

	return &DailyReportJSON{
		Date:           date,
		MarketSnapshot: marketSnapshot,
		Portfolio:      portfolioJSON,
		Rebalance:      rebalanceJSON,
		StrategyAdvice: strategyJSON,
		News:           newsJSON,
		ActionItems:    actionItems,
	}, nil
}

// buildActionItems 构建操作清单
func (s *Service) buildActionItems(
	signals []model.Signal,
	portfolio *PortfolioJSON,
	marketSnapshot *MarketSnapshotJSON,
) []ActionItemJSON {
	var items []ActionItemJSON

	// 开盘前检查
	items = append(items, ActionItemJSON{
		Time:     "09:25",
		Action:   "检查",
		Detail:   "查看隔夜新闻、外围市场、集合竞价情况",
		Priority: 1,
	})

	// 开盘后: 卖出优先
	for _, sig := range signals {
		if sig.Direction == model.DirSell {
			items = append(items, ActionItemJSON{
				Time:     "09:30",
				Action:   "卖出",
				TsCode:   sig.TsCode,
				Name:     s.stockName(sig.TsCode),
				Detail:   sig.Reason,
				Priority: 1,
			})
		}
	}

	// 盘中: 告警检查
	if marketSnapshot != nil {
		for _, alarm := range marketSnapshot.Alarms {
			priority := 3
			if alarm["level"] == "danger" {
				priority = 1
			} else if alarm["level"] == "warning" {
				priority = 2
			}
			items = append(items, ActionItemJSON{
				Time:     "盘中",
				Action:   "检查",
				TsCode:   alarm["ts_code"],
				Detail:   alarm["message"],
				Priority: priority,
			})
		}
	}

	// 盘中: 买入
	for _, sig := range signals {
		if sig.Direction == model.DirBuy {
			items = append(items, ActionItemJSON{
				Time:     "盘中",
				Action:   "买入",
				TsCode:   sig.TsCode,
				Name:     s.stockName(sig.TsCode),
				Detail:   sig.Reason,
				Priority: 3,
			})
		}
	}

	// 尾盘
	items = append(items, ActionItemJSON{
		Time:     "14:50",
		Action:   "检查",
		Detail:   "检查未完成订单, 决定是否留仓过夜",
		Priority: 2,
	})

	// 盘后
	items = append(items, ActionItemJSON{
		Time:     "盘后",
		Action:   "检查",
		Detail:   "复盘当日操作, 更新交易日志",
		Priority: 3,
	})

	return items
}
