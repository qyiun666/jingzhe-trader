package indicator

import "math"

// Mean 计算算术平均值
// 输入为空时返回 NaN
func Mean(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return math.NaN()
	}
	sum := 0.0
	for _, v := range values {
		sum += v
	}
	return sum / float64(n)
}

// Variance 计算总体方差
// 输入为空时返回 NaN
func Variance(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return math.NaN()
	}
	m := Mean(values)
	sum := 0.0
	for _, v := range values {
		d := v - m
		sum += d * d
	}
	return sum / float64(n)
}

// StdDev 计算总体标准差
// 输入为空时返回 NaN
func StdDev(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	return math.Sqrt(Variance(values))
}

// Max 返回序列中的最大值
// 输入为空时返回 NaN
func Max(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return math.NaN()
	}
	m := values[0]
	for i := 1; i < n; i++ {
		if values[i] > m {
			m = values[i]
		}
	}
	return m
}

// Min 返回序列中的最小值
// 输入为空时返回 NaN
func Min(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return math.NaN()
	}
	m := values[0]
	for i := 1; i < n; i++ {
		if values[i] < m {
			m = values[i]
		}
	}
	return m
}

// Highest 返回每个位置过去 period 个值(含当前)的最高值
// 输出长度与输入相同, 前 period-1 个为 NaN
// period 不合法(<1)或大于输入长度时返回全 NaN
func Highest(values []float64, period int) []float64 {
	n := len(values)
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = math.NaN()
	}
	if period < 1 || n == 0 || period > n {
		return out
	}
	for i := period - 1; i < n; i++ {
		out[i] = Max(values[i-period+1 : i+1])
	}
	return out
}

// Lowest 返回每个位置过去 period 个值(含当前)的最低值
// 输出长度与输入相同, 前 period-1 个为 NaN
// period 不合法(<1)或大于输入长度时返回全 NaN
func Lowest(values []float64, period int) []float64 {
	n := len(values)
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = math.NaN()
	}
	if period < 1 || n == 0 || period > n {
		return out
	}
	for i := period - 1; i < n; i++ {
		out[i] = Min(values[i-period+1 : i+1])
	}
	return out
}

// nanSlice 生成长度为 n 的全 NaN 切片
func nanSlice(n int) []float64 {
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = math.NaN()
	}
	return out
}

// isNaN 判断是否为 NaN
func isNaN(v float64) bool {
	return v != v
}
