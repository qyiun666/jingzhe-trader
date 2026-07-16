package indicator

import "math"

// ATR 平均真实波幅 (用于止损)
// 默认参数: period=14
// TR = max(high-low, |high-prev_close|, |low-prev_close|)
// ATR = Wilder 平滑法对 TR 求平均
// 返回长度与输入相同, 前 period-1 个为 NaN
// highs, lows, closes 三者长度必须一致
func ATR(highs, lows, closes []float64, period int) []float64 {
	n := len(closes)
	out := nanSlice(n)

	// 输入校验
	if n == 0 || len(highs) != n || len(lows) != n || period < 1 || period > n {
		return out
	}

	// 计算真实波幅 TR
	tr := make([]float64, n)
	// 第一个元素没有前收盘价, TR = high - low
	tr[0] = highs[0] - lows[0]
	for i := 1; i < n; i++ {
		hl := highs[i] - lows[i]
		hpc := math.Abs(highs[i] - closes[i-1])
		lpc := math.Abs(lows[i] - closes[i-1])
		tr[i] = math.Max(hl, math.Max(hpc, lpc))
	}

	// 第一个 ATR = 前 period 个 TR 的 SMA (位于 index = period-1)
	sum := 0.0
	for i := 0; i < period; i++ {
		sum += tr[i]
	}
	prev := sum / float64(period)
	out[period-1] = prev

	// 后续使用 Wilder 平滑: ATR = (prevATR*(period-1) + currentTR) / period
	for i := period; i < n; i++ {
		prev = (prev*float64(period-1) + tr[i]) / float64(period)
		out[i] = prev
	}

	return out
}
