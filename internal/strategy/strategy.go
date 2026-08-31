package strategy

import (
	"context"
	"math"

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
	TradeDate  string                     // 当前交易日期 YYYYMMDD
	Universe   []string                   // 当前股票池
	Bars       map[string]*model.Bar      // 当日各标的行情 (前复权)
	Positions  map[string]*model.Position // 当前持仓
	Cash       float64                    // 可用现金
	TotalAsset float64                    // 总资产
	History    HistoryProvider            // 历史数据访问器
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

// ==================== 策略公共 helper ====================

// paramFloat 读取浮点参数, 不存在或类型不符时返回默认值
func paramFloat(params map[string]interface{}, key string, def float64) float64 {
	if v, ok := params[key]; ok {
		if n, ok := v.(float64); ok {
			return n
		}
	}
	return def
}

// paramInt 读取整数参数 (YAML 数字解析为 float64)
func paramInt(params map[string]interface{}, key string, def int) int {
	return int(paramFloat(params, key, float64(def)))
}

// paramBool 读取布尔参数
func paramBool(params map[string]interface{}, key string, def bool) bool {
	if v, ok := params[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

// paramStr 读取字符串参数
func paramStr(params map[string]interface{}, key string, def string) string {
	if v, ok := params[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

// CalcBuyQty 按仓位占比计算买入数量 (向下取整到100股)
// 导出供辩论增强复用: LLM 给出的仓位占比必须按与策略下单完全相同的口径换算, 否则两处规则会漂移
func CalcBuyQty(totalAsset, price, pct float64) int {
	if totalAsset <= 0 || price <= 0 || pct <= 0 {
		return 0
	}
	return int(totalAsset*pct/price/100) * 100
}

// buySignal 构造买入信号
func buySignal(tsCode string, qty int, reason string, strength float64) model.Signal {
	return model.Signal{TsCode: tsCode, Direction: model.DirBuy, TargetQty: qty, Reason: reason, Strength: strength}
}

// sellSignal 构造卖出信号
func sellSignal(tsCode string, qty int, reason string, strength float64) model.Signal {
	return model.Signal{TsCode: tsCode, Direction: model.DirSell, TargetQty: qty, Reason: reason, Strength: strength}
}

// crossUp 快速线上穿慢速线 (金叉)
func crossUp(prevFast, prevSlow, currFast, currSlow float64) bool {
	return prevFast <= prevSlow && currFast > currSlow
}

// crossDown 快速线下穿慢速线 (死叉)
func crossDown(prevFast, prevSlow, currFast, currSlow float64) bool {
	return prevFast >= prevSlow && currFast < currSlow
}

// tail2Valid 判断序列末尾两个值是否有效 (非 NaN)
func tail2Valid(vals []float64) bool {
	n := len(vals)
	return n >= 2 && !math.IsNaN(vals[n-1]) && !math.IsNaN(vals[n-2])
}

// closesOf 提取收盘价序列 (前复权: 序列已由 DataProvider/dbHistoryAdapter 复权, 价格即复权价)
func closesOf(bars []model.Bar) []float64 {
	closes := make([]float64, len(bars))
	for i, bar := range bars {
		closes[i] = bar.Close
	}
	return closes
}

// highsLowsOf 提取最高/最低价序列 (前复权: 同 closesOf)
func highsLowsOf(bars []model.Bar) ([]float64, []float64) {
	highs := make([]float64, len(bars))
	lows := make([]float64, len(bars))
	for i, bar := range bars {
		highs[i] = bar.High
		lows[i] = bar.Low
	}
	return highs, lows
}
