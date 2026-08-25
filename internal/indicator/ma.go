package indicator

import talib "github.com/markcheno/go-talib"

// SMA 简单移动平均
// 委托 go-talib 实现 (数值已验证一致); 输入含 NaN 或参数越界时回退自研
// 返回长度与输入相同的切片, 前 period-1 个为 NaN
func SMA(values []float64, period int) []float64 {
	n := len(values)
	if period < 1 || n == 0 || period > n {
		return nanSlice(n)
	}
	if hasNaN(values) {
		return smaLocal(values, period)
	}
	return padTo(talib.Sma(values, period), n, period-1)
}

// smaLocal 自研实现 (输入含 NaN 时回退, 与旧行为完全一致)
func smaLocal(values []float64, period int) []float64 {
	n := len(values)
	out := nanSlice(n)
	if period < 1 || n == 0 || period > n {
		return out
	}

	// 滑动窗口求和, 避免重复计算
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += values[i]
	}
	out[period-1] = sum / float64(period)

	for i := period; i < n; i++ {
		sum += values[i] - values[i-period]
		out[i] = sum / float64(period)
	}
	return out
}

// EMA 指数移动平均
// 平滑系数 k = 2 / (period + 1)
// EMA今日 = EMA昨日 * (1-k) + 今日价格 * k
// 第一个有效值使用前 period 个值的 SMA 作为初始值
// 返回长度与输入相同, 前 period-1 个为 NaN
func EMA(values []float64, period int) []float64 {
	n := len(values)
	out := nanSlice(n)
	if period < 1 || n == 0 || period > n {
		return out
	}

	// 平滑系数
	k := 2.0 / float64(period+1)

	// 使用前 period 个值的 SMA 作为 EMA 的初始值
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += values[i]
	}
	prev := sum / float64(period)
	out[period-1] = prev

	// 从第 period 个开始递推
	for i := period; i < n; i++ {
		prev = prev*(1-k) + values[i]*k
		out[i] = prev
	}
	return out
}

// WMA 加权移动平均
// 委托 go-talib 实现 (数值已验证一致); 输入含 NaN 或参数越界时回退自研
// 权重从 1 到 period 递增 (最新值权重最大为 period, 最旧值权重最小为 1)
// 返回长度与输入相同, 前 period-1 个为 NaN
func WMA(values []float64, period int) []float64 {
	n := len(values)
	if period < 1 || n == 0 || period > n {
		return nanSlice(n)
	}
	if hasNaN(values) {
		return wmaLocal(values, period)
	}
	return padTo(talib.Wma(values, period), n, period-1)
}

// wmaLocal 自研实现 (输入含 NaN 时回退, 与旧行为完全一致)
func wmaLocal(values []float64, period int) []float64 {
	n := len(values)
	out := nanSlice(n)
	if period < 1 || n == 0 || period > n {
		return out
	}

	// 权重总和: 1 + 2 + ... + period = period*(period+1)/2
	weightSum := float64(period) * float64(period+1) / 2.0

	for i := period - 1; i < n; i++ {
		weighted := 0.0
		// 窗口 [i-period+1, i], 最旧值权重为1, 最新值权重为 period
		for j := 0; j < period; j++ {
			// j=0 对应最旧值, 权重为1; j=period-1 对应最新值, 权重为 period
			weighted += values[i-period+1+j] * float64(j+1)
		}
		out[i] = weighted / weightSum
	}
	return out
}
