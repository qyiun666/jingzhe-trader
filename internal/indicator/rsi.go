package indicator

// RSI 相对强弱指标
// period 通常用 6, 12, 24
// 计算方法 (Wilder 平滑法):
//  1. 计算每日变动 delta = close[i] - close[i-1]
//  2. 分离 gain(正) 和 loss(负的绝对值)
//  3. 用 Wilder 平滑法计算平均增益和平均损失
//  4. RS = 平均增益 / 平均损失
//  5. RSI = 100 - 100/(1+RS)
//
// 返回长度与输入相同, 前 period 个为 NaN (首个有效值在 index=period)
func RSI(values []float64, period int) []float64 {
	n := len(values)
	out := nanSlice(n)
	if period < 1 || n < period+1 {
		return out
	}

	// 计算每日变动并分离涨跌
	gains := make([]float64, n) // gain[i] 对应 close[i]-close[i-1] 的正部分
	losses := make([]float64, n) // loss[i] 对应 close[i]-close[i-1] 的负部分的绝对值
	for i := 1; i < n; i++ {
		diff := values[i] - values[i-1]
		if diff >= 0 {
			gains[i] = diff
		} else {
			losses[i] = -diff
		}
	}

	// 第一个平均增益/损失: 使用 index 1..period 的 SMA
	sumGain := 0.0
	sumLoss := 0.0
	for i := 1; i <= period; i++ {
		sumGain += gains[i]
		sumLoss += losses[i]
	}
	avgGain := sumGain / float64(period)
	avgLoss := sumLoss / float64(period)

	// 首个 RSI 在 index = period
	out[period] = rsiFromAvg(avgGain, avgLoss)

	// 后续使用 Wilder 平滑: avg = (prevAvg*(period-1) + current) / period
	for i := period + 1; i < n; i++ {
		avgGain = (avgGain*float64(period-1) + gains[i]) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + losses[i]) / float64(period)
		out[i] = rsiFromAvg(avgGain, avgLoss)
	}

	return out
}

// rsiFromAvg 根据平均增益和平均损失计算 RSI 值
// 除零保护: avgLoss 为 0 时 RSI = 100; 两者均 0 时 RSI = 50
func rsiFromAvg(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		if avgGain == 0 {
			// 涨跌均为 0, 视为中性
			return 50
		}
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs)
}
