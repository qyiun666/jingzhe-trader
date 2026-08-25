package indicator

import talib "github.com/markcheno/go-talib"

// BollResult 布林带计算结果
type BollResult struct {
	Upper  []float64 // 上轨
	Middle []float64 // 中轨 (SMA)
	Lower  []float64 // 下轨
}

// Bollinger Bands 布林带
// 委托 go-talib 实现 (总体标准差口径与自研一致, 数值已验证); 输入含 NaN 或参数越界时回退自研
// 默认参数: period=20, multiplier=2.0
// 中轨 = SMA(period)
// 标准差 = 过去 period 个值的总体标准差
// 上轨 = 中轨 + multiplier * 标准差
// 下轨 = 中轨 - multiplier * 标准差
// 三个切片长度均与输入相同, 前 period-1 个为 NaN
func Boll(values []float64, period int, multiplier float64) BollResult {
	n := len(values)
	if period < 1 || n == 0 || period > n {
		return BollResult{Upper: nanSlice(n), Middle: nanSlice(n), Lower: nanSlice(n)}
	}
	if hasNaN(values) {
		return bollLocal(values, period, multiplier)
	}
	upper, middle, lower := talib.BBands(values, period, multiplier, multiplier, talib.SMA)
	return BollResult{
		Upper:  padTo(upper, n, period-1),
		Middle: padTo(middle, n, period-1),
		Lower:  padTo(lower, n, period-1),
	}
}

// bollLocal 自研实现 (输入含 NaN 时回退, 与旧行为完全一致)
func bollLocal(values []float64, period int, multiplier float64) BollResult {
	n := len(values)
	upper := nanSlice(n)
	middle := nanSlice(n)
	lower := nanSlice(n)

	if period < 1 || n == 0 || period > n {
		return BollResult{Upper: upper, Middle: middle, Lower: lower}
	}

	// 中轨 = SMA(period)
	middle = SMA(values, period)

	// 上轨/下轨: 基于过去 period 个值的标准差
	for i := period - 1; i < n; i++ {
		sd := StdDev(values[i-period+1 : i+1])
		m := middle[i]
		upper[i] = m + multiplier*sd
		lower[i] = m - multiplier*sd
	}

	return BollResult{Upper: upper, Middle: middle, Lower: lower}
}
