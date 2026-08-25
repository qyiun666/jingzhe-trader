package indicator

// 基于 go-talib 的扩展指标 (自研未实现的指标, 供策略/因子扩展使用)
// 与项目契约一致: 输出等长, 无效位置为 NaN; 输入含 NaN 或参数越界时返回全 NaN

import talib "github.com/markcheno/go-talib"

// ADX 平均趋向指数 (period 默认 14)
// 有效起点: index=2*period-1 (talib 内部 lookback = 2*period-1, 前 2*period-1 个为 NaN)
func ADX(highs, lows, closes []float64, period int) []float64 {
	n := len(closes)
	if period < 1 || n == 0 || period*2-1 > n || len(highs) != n || len(lows) != n {
		return nanSlice(n)
	}
	if hasNaN(highs) || hasNaN(lows) || hasNaN(closes) {
		return nanSlice(n)
	}
	return padTo(talib.Adx(highs, lows, closes, period), n, 2*period-1)
}

// CCI 顺势指标 (period 默认 20)
// 有效起点: index=period-1
func CCI(highs, lows, closes []float64, period int) []float64 {
	n := len(closes)
	if period < 1 || n == 0 || period > n || len(highs) != n || len(lows) != n {
		return nanSlice(n)
	}
	if hasNaN(highs) || hasNaN(lows) || hasNaN(closes) {
		return nanSlice(n)
	}
	return padTo(talib.Cci(highs, lows, closes, period), n, period-1)
}

// OBV 能量潮 (无周期参数, 输出全部有效)
func OBV(closes, volumes []float64) []float64 {
	n := len(closes)
	if n == 0 || len(volumes) != n {
		return nanSlice(n)
	}
	if hasNaN(closes) || hasNaN(volumes) {
		return nanSlice(n)
	}
	return talib.Obv(closes, volumes)
}

// ROC 变动率 (period 默认 12)
// 有效起点: index=period (前 period 个为 NaN)
func ROC(values []float64, period int) []float64 {
	n := len(values)
	if period < 1 || n == 0 || period > n {
		return nanSlice(n)
	}
	if hasNaN(values) {
		return nanSlice(n)
	}
	return padTo(talib.Roc(values, period), n, period)
}

// STOCHResult 随机指标计算结果
type STOCHResult struct {
	K []float64 // 慢速K线
	D []float64 // 慢速D线 (信号线)
}

// STOCH 慢速随机指标 (K/D 均用 SMA 平滑)
// 有效起点: index = fastK + slowK + slowD - 3
func STOCH(highs, lows, closes []float64, fastK, slowK, slowD int) STOCHResult {
	n := len(closes)
	k := nanSlice(n)
	d := nanSlice(n)
	if fastK < 1 || slowK < 1 || slowD < 1 || n == 0 || len(highs) != n || len(lows) != n {
		return STOCHResult{K: k, D: d}
	}
	lookback := fastK + slowK + slowD - 3
	if lookback >= n {
		return STOCHResult{K: k, D: d}
	}
	if hasNaN(highs) || hasNaN(lows) || hasNaN(closes) {
		return STOCHResult{K: k, D: d}
	}
	slowKv, slowDv := talib.Stoch(highs, lows, closes, fastK, slowK, talib.SMA, slowD, talib.SMA)
	return STOCHResult{
		K: padTo(slowKv, n, lookback),
		D: padTo(slowDv, n, lookback),
	}
}
