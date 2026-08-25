package api

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// RunStrategy 策略建议
func (s *Service) RunStrategy(date string, strategyName string) (*StrategyJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	positions := s.getPositions()
	asset := s.getAsset()
	s.brk.UpdateMarketValue(todayBars)
	positions, _ = s.brk.QueryPositions()
	asset, _ = s.brk.QueryAsset()

	signals, sigErr := s.runStrategy(date, strategyName, todayBars, positions, asset)
	if sigErr != nil {
		logger.L().Errorf("[%s] 策略建议信号生成失败: %v", date, sigErr)
	}
	return s.buildStrategyJSON(date, signals, todayBars, nil), nil
}

// buildStrategyJSON 构建策略建议 JSON
func (s *Service) buildStrategyJSON(
	date string,
	signals []model.Signal,
	todayBars map[string]*model.Bar,
	portfolio *PortfolioJSON,
) *StrategyJSON {
	// 使用 analysis.AdviseStrategy
	marketBars := make(map[string]*model.Bar)
	for _, code := range []string{"000001.SH", "399001.SZ", "000300.SH"} {
		if bar, ok := todayBars[code]; ok {
			marketBars[code] = bar
		}
	}

	// 简化: 无历史收益率数据, 使用空数组
	strategyPerformances := make(map[string]analysis.StrategyPerformance)
	advice := analysis.AdviseStrategy(date, marketBars, nil, strategyPerformances)

	return &StrategyJSON{
		Recommended: advice.RecommendedStrategy,
		Confidence:  advice.Confidence,
		Reason:      advice.Reason,
		Condition:   advice.MarketCondition,
	}
}

// getStrategy 获取缓存的策略实例 (首次调用时创建并初始化, 后续复用以保留内部状态)
func (s *Service) getStrategy(name string) (strategy.Strategy, bool) {
	// 先读缓存 (快速路径)
	s.strategyCacheMu.RLock()
	if strat, ok := s.strategyCache[name]; ok {
		s.strategyCacheMu.RUnlock()
		return strat, true
	}
	s.strategyCacheMu.RUnlock()

	// 缓存未命中: 从注册表创建
	reg := strategy.DefaultRegistry()
	strat, ok := reg.Get(name)
	if !ok {
		return nil, false
	}
	// 初始化策略参数 (仅首次)
	if err := strat.Init(context.Background(), s.cfg.StrategyParams(name)); err != nil {
		logger.L().Errorf("策略 %s 初始化失败: %v", name, err)
		return nil, false
	}

	// 写入缓存
	s.strategyCacheMu.Lock()
	s.strategyCache[name] = strat
	s.strategyCacheMu.Unlock()
	return strat, true
}

// runStrategy 运行策略产生信号
// universe 限定为配置股票池 + 当前持仓 (与回测一致, 避免全市场扫描)
func (s *Service) runStrategy(
	date string,
	strategyName string,
	bars map[string]*model.Bar,
	positions map[string]*model.Position,
	asset *broker.AssetInfo,
) ([]model.Signal, error) {
	strat, ok := s.getStrategy(strategyName)
	if !ok {
		return nil, fmt.Errorf("策略不存在: %s", strategyName)
	}

	// 股票池: 配置 universe + 持仓, 且当日有行情
	seen := make(map[string]bool)
	var universe []string
	for _, code := range s.cfg.UniverseCodes() {
		if !seen[code] && bars[code] != nil {
			seen[code] = true
			universe = append(universe, code)
		}
	}
	for code := range positions {
		if !seen[code] && bars[code] != nil {
			seen[code] = true
			universe = append(universe, code)
		}
	}
	// 合并自动选股结果 (选股器每日自动发现的候选股票)
	if s.screenRepo != nil {
		if screenedCodes, err := s.screenRepo.GetScreenedCodes(); err == nil {
			for _, code := range screenedCodes {
				if !seen[code] && bars[code] != nil {
					seen[code] = true
					universe = append(universe, code)
				}
			}
		}
	}
	if len(universe) == 0 {
		logger.L().Warnf("[%s] 股票池为空(配置 universe 无当日行情), 策略跳过", date)
		return nil, nil
	}

	barCtx := &strategy.BarContext{
		TradeDate:  date,
		Universe:   universe,
		Bars:       bars,
		Positions:  positions,
		Cash:       asset.Cash,
		TotalAsset: asset.TotalAsset,
		History:    &dbHistoryAdapter{barRepo: s.barRepo}, // 真实历史K线, 均线类策略依赖
	}

	// 直接调用 OnBar (策略已在 getStrategy 中初始化, 不再每次重置状态)
	signals, err := strat.OnBar(context.Background(), barCtx)
	if err != nil {
		// 策略执行失败不得静默: 上抛错误, 让信号任务失败并告警, 而不是产出"无计划"的假正常结果
		return nil, fmt.Errorf("策略 %s 执行失败: %w", strategyName, err)
	}
	return signals, nil
}
