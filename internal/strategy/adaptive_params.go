package strategy

import (
	"jingzhe-trader/internal/indicator"
)

// AdaptiveParams 自适应参数调整器
// 根据市场波动率(ATR)动态调整策略参数
// 高波动市场: 放宽止损、拉长均线周期、降低仓位
// 低波动市场: 收紧止损、缩短均线周期、提高仓位
type AdaptiveParams struct {
	// 基准参数 (中等波动环境下的默认值)
	baseShortMA     int     // 短均线基准周期
	baseLongMA      int     // 长均线基准周期
	baseStopLoss    float64 // 基准止损比例 (负值, 如 -0.15)
	baseTakeProfit  float64 // 基准止盈比例 (正值, 如 0.30)
	basePositionPct float64 // 基准单票仓位

	// 波动率状态
	atrHistory []float64 // 历史ATR百分比序列 (ATR/收盘价, 用于归一化)
	atrPeriod  int       // ATR计算周期
}

// NewAdaptiveParams 创建自适应参数调整器
func NewAdaptiveParams() *AdaptiveParams {
	return &AdaptiveParams{
		baseShortMA:     5,
		baseLongMA:      20,
		baseStopLoss:    -0.15,
		baseTakeProfit:  0.30,
		basePositionPct: 0.10,
		atrPeriod:       14,
	}
}

// SetBaseParams 设置基准参数
// shortMA: 短均线基准周期
// longMA: 长均线基准周期
// stopLoss: 基准止损比例 (负值)
// takeProfit: 基准止盈比例 (正值)
// positionPct: 基准单票仓位占比
func (ap *AdaptiveParams) SetBaseParams(shortMA, longMA int, stopLoss, takeProfit, positionPct float64) {
	ap.baseShortMA = shortMA
	ap.baseLongMA = longMA
	ap.baseStopLoss = stopLoss
	ap.baseTakeProfit = takeProfit
	ap.basePositionPct = positionPct
}

// Update 更新波动率状态 (每个交易日调用)
// closes: 标的收盘价序列 (至少需要 atrPeriod + 1 根)
// highs, lows: 最高价、最低价序列
func (ap *AdaptiveParams) Update(closes, highs, lows []float64) {
	if len(closes) < ap.atrPeriod+1 || len(highs) < ap.atrPeriod+1 || len(lows) < ap.atrPeriod+1 {
		return
	}
	// 计算ATR
	atr := indicator.ATR(highs, lows, closes, ap.atrPeriod)
	n := len(atr)
	if n > 0 && !isNaNFloat(atr[n-1]) {
		currentAtr := atr[n-1]
		// 用ATR/收盘价归一化 (波动率百分比)
		lastClose := closes[len(closes)-1]
		if lastClose > 0 {
			atrPct := currentAtr / lastClose
			ap.atrHistory = append(ap.atrHistory, atrPct)
			// 只保留最近250天 (约1年)
			if len(ap.atrHistory) > 250 {
				ap.atrHistory = ap.atrHistory[len(ap.atrHistory)-250:]
			}
		}
	}
}

// volatilityLevel 波动率等级: 0=极低, 1=低, 2=中, 3=高, 4=极高
// 基于历史ATR分位数划分
func (ap *AdaptiveParams) volatilityLevel() int {
	if len(ap.atrHistory) < 20 {
		return 2 // 数据不足, 默认中等
	}
	current := ap.atrHistory[len(ap.atrHistory)-1]
	// 计算当前波动率在历史中的分位数
	var belowCount int
	for _, v := range ap.atrHistory {
		if v < current {
			belowCount++
		}
	}
	percentile := float64(belowCount) / float64(len(ap.atrHistory))
	switch {
	case percentile < 0.2:
		return 0 // 极低
	case percentile < 0.4:
		return 1 // 低
	case percentile < 0.6:
		return 2 // 中
	case percentile < 0.8:
		return 3 // 高
	default:
		return 4 // 极高
	}
}

// GetMA 动态调整均线周期
// 高波动: 拉长周期 (过滤噪音)
// 低波动: 缩短周期 (更灵敏)
// 返回调整后的短周期和长周期
func (ap *AdaptiveParams) GetMA() (shortMA, longMA int) {
	level := ap.volatilityLevel()
	// 波动率等级 0-4 对应调整系数 0.8 ~ 1.4
	scale := 0.8 + float64(level)*0.15
	shortMA = maxInt(3, int(float64(ap.baseShortMA)*scale))
	longMA = maxInt(10, int(float64(ap.baseLongMA)*scale))
	return
}

// GetStopLoss 动态调整止损比例
// 高波动: 放宽止损 (避免被洗)
// 低波动: 收紧止损 (锁定利润)
// 返回止损比例 (负值, 如 -0.12 表示 -12%)
func (ap *AdaptiveParams) GetStopLoss() float64 {
	level := ap.volatilityLevel()
	// 波动率等级 0-4 对应止损系数 0.6 ~ 1.6
	// 基准 -15% 时, 范围约 -9% ~ -24%
	return ap.baseStopLoss * (0.6 + float64(level)*0.25)
}

// GetTakeProfit 动态调整止盈比例
// 高波动: 目标更高 (波动大利润空间大)
// 低波动: 目标更低 (见好就收)
// 返回止盈比例 (正值, 如 0.30 表示 30%)
func (ap *AdaptiveParams) GetTakeProfit() float64 {
	level := ap.volatilityLevel()
	// 波动率等级 0-4 对应止盈系数 0.7 ~ 1.5
	// 基准 30% 时, 范围约 21% ~ 45%
	return ap.baseTakeProfit * (0.7 + float64(level)*0.2)
}

// GetPositionSize 动态调整仓位
// 高波动: 降低仓位 (控制风险)
// 低波动: 增加仓位 (提高收益)
// 返回仓位占比 (如 0.10 表示 10%)
func (ap *AdaptiveParams) GetPositionSize() float64 {
	level := ap.volatilityLevel()
	// 波动率等级 0-4 对应仓位系数 1.3 ~ 0.5
	// 基准 10% 时, 范围约 13% ~ 5%
	scale := 1.3 - float64(level)*0.2
	return ap.basePositionPct * scale
}

// ParamStatus 参数状态 (用于日志/调试)
type ParamStatus struct {
	VolatilityLevel int     `json:"volatility_level"` // 0-4
	VolatilityDesc  string  `json:"volatility_desc"`  // 极低/低/中/高/极高
	ShortMA         int     `json:"short_ma"`
	LongMA          int     `json:"long_ma"`
	StopLoss        float64 `json:"stop_loss"`
	TakeProfit      float64 `json:"take_profit"`
	PositionPct     float64 `json:"position_pct"`
}

// GetStatus 获取当前参数状态
func (ap *AdaptiveParams) GetStatus() ParamStatus {
	level := ap.volatilityLevel()
	shortMA, longMA := ap.GetMA()
	descs := []string{"极低波动", "低波动", "中等波动", "高波动", "极高波动"}
	return ParamStatus{
		VolatilityLevel: level,
		VolatilityDesc:  descs[level],
		ShortMA:         shortMA,
		LongMA:          longMA,
		StopLoss:        ap.GetStopLoss(),
		TakeProfit:      ap.GetTakeProfit(),
		PositionPct:     ap.GetPositionSize(),
	}
}

// isNaNFloat 判断是否为 NaN
func isNaNFloat(v float64) bool {
	return v != v
}

// maxInt 返回两个整数中的较大值
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
