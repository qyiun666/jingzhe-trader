package strategy

import (
	"context"

	"jingzhe-trader/internal/model"
)

// Strategy 策略接口
// 策略一次编写, 回测和实盘共用同一接口
type Strategy interface {
	// Name 策略名称
	Name() string
	// Init 初始化策略参数
	Init(ctx context.Context, params map[string]interface{}) error
	// OnBar 每个交易日触发, 接收行情和持仓上下文, 返回交易信号
	OnBar(ctx context.Context, barCtx *BarContext) ([]model.Signal, error)
}

// BarContext 策略上下文, 聚合策略所需的全部信息
type BarContext struct {
	TradeDate string                       // 当前交易日期 YYYYMMDD
	Universe  []string                     // 当前股票池
	Bars      map[string]*model.Bar        // 当日各标的行情 (前复权)
	Positions map[string]*model.Position   // 当前持仓
	Cash      float64                      // 可用现金
	TotalAsset float64                     // 总资产
	History   HistoryProvider              // 历史数据访问器
}

// HistoryProvider 历史数据提供者
// 策略通过此接口获取历史K线序列, 用于计算技术指标
type HistoryProvider interface {
	// GetBars 获取指定股票截至 endDate 的 N 根日线 (含 endDate 当日, 前复权)
	GetBars(tsCode, endDate string, n int) ([]model.Bar, error)
	// GetCloses 获取指定股票截至 endDate 的 N 个收盘价 (前复权)
	GetCloses(tsCode, endDate string, n int) ([]float64, error)
}

// Registry 策略注册表
type Registry struct {
	strategies map[string]func() Strategy
}

// NewRegistry 创建策略注册表
func NewRegistry() *Registry {
	return &Registry{strategies: make(map[string]func() Strategy)}
}

// Register 注册策略构造函数
func (r *Registry) Register(name string, factory func() Strategy) {
	r.strategies[name] = factory
}

// Get 获取策略实例
func (r *Registry) Get(name string) (Strategy, bool) {
	factory, ok := r.strategies[name]
	if !ok {
		return nil, false
	}
	return factory(), true
}

// Names 返回所有已注册策略名
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.strategies))
	for name := range r.strategies {
		names = append(names, name)
	}
	return names
}

// DefaultRegistry 默认策略注册表 (包含内置策略)
func DefaultRegistry() *Registry {
	r := NewRegistry()
	r.Register("ma_cross", func() Strategy { return &MACrossStrategy{} })
	r.Register("macd", func() Strategy { return &MACDStrategy{} })
	r.Register("boll_breakout", func() Strategy { return &BollBreakoutStrategy{} })
	r.Register("multi_factor", func() Strategy { return NewMultiFactorStrategy() })
	r.Register("intraday_t", func() Strategy { return NewIntradayTStrategy() })
	return r
}
