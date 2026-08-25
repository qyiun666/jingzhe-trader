package api

import (
	"fmt"
	"sync"
	"time"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// ==================== 动态策略 ====================

// advisorAdapter 将 analysis.AdviseStrategy 包装为 strategy.StrategyAdvisor 接口
// 输入真实数据:
//   - 沪深300近30个交易日收益序列 (市场环境判断基于趋势而非单日涨跌)
//   - 各策略最近一次回测run的绩效 (夏普/胜率/回撤, 来自 trades 归因, 缓存1小时)
type advisorAdapter struct {
	barRepo   *store.BarRepo
	tradeRepo *store.TradeRepo
	mu        sync.Mutex
	cache     map[string]analysis.StrategyPerformance
	cachedAt  time.Time
}

func (a *advisorAdapter) Advise(date string, indexBars map[string]*model.Bar) *strategy.AdvisorResult {
	recentReturns := a.recentIndexReturns(date)
	advice := analysis.AdviseStrategy(date, indexBars, recentReturns, a.strategyPerformances())
	return &strategy.AdvisorResult{
		RecommendedStrategy: advice.RecommendedStrategy,
		MarketCondition:     advice.MarketCondition,
		Confidence:          advice.Confidence,
	}
}

// strategyPerformances 各策略最近一次回测run的真实绩效 (缓存1小时, 回测数据静态)
func (a *advisorAdapter) strategyPerformances() map[string]analysis.StrategyPerformance {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.cachedAt) < time.Hour && a.cache != nil {
		return a.cache
	}
	out := make(map[string]analysis.StrategyPerformance)
	if a.tradeRepo != nil {
		runs, err := a.tradeRepo.GetLatestRunPerStrategy()
		if err != nil {
			logger.L().Warnw("策略业绩查询失败, 使用中性基准", "err", err)
		} else {
			for strat, runID := range runs {
				snaps, err1 := a.tradeRepo.GetAccountSnapshotsByRunID(runID)
				trades, err2 := a.tradeRepo.GetTradesByRunID(runID)
				if err1 != nil || err2 != nil || len(snaps) < 2 || len(trades) == 0 {
					continue
				}
				m := backtest.CalculateMetrics(snaps, trades, nil)
				out[strat] = analysis.StrategyPerformance{
					Name:        strat,
					TotalReturn: m.TotalReturn,
					Sharpe:      m.SharpeRatio,
					MaxDrawdown: m.MaxDrawdown,
					WinRate:     m.WinRate,
				}
			}
		}
	}
	a.cache = out
	a.cachedAt = time.Now()
	return out
}

// recentIndexReturns 沪深300最近30个交易日的日收益率序列 (前复权口径, 与策略一致)
func (a *advisorAdapter) recentIndexReturns(date string) []float64 {
	if a == nil || a.barRepo == nil {
		return nil
	}
	bars, err := a.barRepo.GetBars("000300.SH", "", date)
	if err != nil {
		return nil
	}
	if len(bars) < 2 {
		return nil
	}
	// 取最近30根
	if len(bars) > 30 {
		bars = bars[len(bars)-30:]
	}
	// 前复权后再算收益率, 与 DataProvider 同口径
	model.AdjustBarsForward(bars)
	returns := make([]float64, 0, len(bars)-1)
	for i := 1; i < len(bars); i++ {
		if bars[i-1].Close > 0 {
			returns = append(returns, bars[i].Close/bars[i-1].Close-1)
		}
	}
	return returns
}

// BuildStrategyStatus returns the dynamic strategy selector status.
func (s *Service) BuildStrategyStatus() (interface{}, error) {
	if s.dynamicSelector == nil {
		return nil, fmt.Errorf("动态策略选择器未启用")
	}
	return s.dynamicSelector.GetStatus(), nil
}

// SwitchStrategy switches the active strategy used for signal generation.
func (s *Service) SwitchStrategy(name string) (map[string]string, error) {
	if name == "" {
		return nil, fmt.Errorf("请指定策略名称")
	}

	// 验证策略存在并确保缓存中有实例
	strat, ok := s.getStrategy(name)
	if !ok {
		return nil, fmt.Errorf("未知策略: %s", name)
	}

	// 通过动态选择器执行切换
	if s.dynamicSelector != nil {
		s.dynamicSelector.SwitchTo(name, strat)
	}

	return map[string]string{
		"message":  "策略已切换为 " + name,
		"strategy": name,
	}, nil
}
