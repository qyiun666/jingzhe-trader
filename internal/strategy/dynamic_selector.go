package strategy

import (
	"context"
	"sync"

	"jingzhe-trader/internal/model"
)

// AdvisorResult 策略建议结果
// 用于解耦 strategy 包和 analysis 包, 避免循环依赖
type AdvisorResult struct {
	RecommendedStrategy  string   // 推荐策略名称（主策略）
	MarketCondition      string   // 市场环境: "牛市" / "下跌" / "震荡"
	Confidence           float64  // 置信度 0-1
	EnhanceStrategies    []string // 建议的增强策略（如做T等持仓增强策略）
}

// StrategyAdvisor 市场策略建议接口
// DynamicSelector 通过此接口获取市场环境判断和策略推荐,
// 具体实现由外部注入 (如 analysis.AdviseStrategy 的包装)
type StrategyAdvisor interface {
	// Advise 根据日期和指数行情, 给出策略建议
	Advise(date string, indexBars map[string]*model.Bar) *AdvisorResult
}

// DynamicSelector 动态策略选择器
// 根据市场环境自动选择最优策略
type DynamicSelector struct {
	mu                sync.RWMutex
	registry          *Registry
	advisor           StrategyAdvisor
	current           Strategy
	currentName       string
	marketCondition   string   // "牛市" / "下跌" / "震荡"
	confidence        float64
	enhanceStrategies []string // 当前建议的增强策略列表
	// 环境检测用的指数代码
	indexCodes []string
}

// NewDynamicSelector 创建动态策略选择器
// advisor: 策略建议器, 用于判断市场环境和推荐策略
func NewDynamicSelector(registry *Registry, advisor StrategyAdvisor) *DynamicSelector {
	ds := &DynamicSelector{
		registry:   registry,
		advisor:    advisor,
		indexCodes: []string{"000001.SH", "399001.SZ", "000300.SH"},
	}
	// 默认使用均线交叉策略
	ds.current, _ = registry.Get("ma_cross")
	ds.currentName = "ma_cross"
	return ds
}

// Select 根据市场环境选择最优策略
// 返回: 策略名称, 是否发生了切换
func (ds *DynamicSelector) Select(date string, marketBars map[string]*model.Bar) (string, bool) {
	ds.mu.Lock()
	defer ds.mu.Unlock()

	// 筛选出指数行情, 用于判断市场环境
	indexBars := make(map[string]*model.Bar)
	for _, code := range ds.indexCodes {
		if bar, ok := marketBars[code]; ok {
			indexBars[code] = bar
		}
	}

	// 调用策略建议器, 获取市场环境判断和推荐策略
	advice := ds.advisor.Advise(date, indexBars)

	recommended := advice.RecommendedStrategy
	ds.marketCondition = advice.MarketCondition
	ds.confidence = advice.Confidence
	ds.enhanceStrategies = ds.resolveEnhanceStrategies(advice)

	// 确认推荐策略在注册表中存在, 执行切换
	if strat, ok := ds.registry.Get(recommended); ok {
		if recommended != ds.currentName {
			// 策略切换: 初始化新策略
			ctx := context.Background()
			_ = strat.Init(ctx, nil)
			ds.current = strat
			ds.currentName = recommended
			return ds.currentName, true // 发生了切换
		}
	}

	return ds.currentName, false // 没有切换
}

// Current 获取当前策略
func (ds *DynamicSelector) Current() Strategy {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.current
}

// CurrentName 获取当前策略名称
func (ds *DynamicSelector) CurrentName() string {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.currentName
}

// MarketCondition 获取当前市场环境
func (ds *DynamicSelector) MarketCondition() string {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.marketCondition
}

// Confidence 获取策略置信度
func (ds *DynamicSelector) Confidence() float64 {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.confidence
}

// SelectorStatus 选择器状态快照
type SelectorStatus struct {
	CurrentStrategy     string   `json:"current_strategy"`      // 当前策略名称
	MarketCondition     string   `json:"market_condition"`      // 市场环境
	Confidence          float64  `json:"confidence"`            // 置信度
	AvailableStrategies []string `json:"available_strategies"`  // 所有可用策略
	EnhanceStrategies   []string `json:"enhance_strategies"`    // 建议的增强策略
}

// GetStatus 获取选择器状态信息
func (ds *DynamicSelector) GetStatus() SelectorStatus {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return SelectorStatus{
		CurrentStrategy:     ds.currentName,
		MarketCondition:     ds.marketCondition,
		Confidence:          ds.confidence,
		AvailableStrategies: ds.registry.Names(),
		EnhanceStrategies:   ds.enhanceStrategies,
	}
}

// EnhanceStrategies 获取当前建议的增强策略列表
func (ds *DynamicSelector) EnhanceStrategies() []string {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	return ds.enhanceStrategies
}

// resolveEnhanceStrategies 根据市场环境解析增强策略建议
// 做T策略(intraday_t)是持仓增强策略，不独立选股，需配合主策略使用
// 适用场景：震荡市效果最佳，利用日内波动降低持仓成本
func (ds *DynamicSelector) resolveEnhanceStrategies(advice *AdvisorResult) []string {
	var enhances []string

	// 如果建议器已经提供了增强策略，直接使用
	if len(advice.EnhanceStrategies) > 0 {
		for _, name := range advice.EnhanceStrategies {
			if _, ok := ds.registry.Get(name); ok {
				enhances = append(enhances, name)
			}
		}
		return enhances
	}

	// 根据市场环境自动推荐增强策略
	switch advice.MarketCondition {
	case "震荡":
		// 震荡市：日内波动频繁，适合用做T策略降低成本
		if _, ok := ds.registry.Get("intraday_t"); ok {
			enhances = append(enhances, "intraday_t")
		}
	case "牛市":
		// 牛市：做T容易卖飞，不主动推荐，但也不禁止
		// （用户有底仓且波动大时仍可使用）
	case "下跌":
		// 下跌市：谨慎做正T，防止越补越跌
		// 倒T（先卖后买）理论上可以用，但风险较高
	}

	return enhances
}