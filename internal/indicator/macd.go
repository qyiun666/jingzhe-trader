package indicator

// MACDResult MACD 指标计算结果
type MACDResult struct {
	DIF       []float64 // 快线 (DIF)
	DEA       []float64 // 慢线 (DEA/信号线)
	Histogram []float64 // MACD 柱 (DIF-DEA)*2
}

// MACD 计算 MACD 指标
// 默认参数: fast=12, slow=26, signal=9
// DIF = EMA(fast) - EMA(slow)
// DEA = EMA(DIF, signal)
// Histogram = (DIF - DEA) * 2
// 三个切片长度均与输入相同, 无效位置为 NaN
func MACD(values []float64, fast, slow, signal int) MACDResult {
	n := len(values)
	dif := nanSlice(n)
	dea := nanSlice(n)
	hist := nanSlice(n)

	// 参数合法性校验
	if n == 0 || fast < 1 || slow < 1 || signal < 1 ||
		fast > n || slow > n || signal > n || fast >= slow {
		return MACDResult{DIF: dif, DEA: dea, Histogram: hist}
	}

	// 计算 fast 和 slow 的 EMA
	emaFast := EMA(values, fast)
	emaSlow := EMA(values, slow)

	// DIF = EMA(fast) - EMA(slow), 仅在两者都有效时才有意义
	// EMA(slow) 从 slow-1 开始有效
	for i := slow - 1; i < n; i++ {
		dif[i] = emaFast[i] - emaSlow[i]
	}

	// DEA = EMA(DIF, signal)
	// DIF 从 slow-1 开始有效, 提取有效部分单独计算 EMA
	validStart := slow - 1
	validLen := n - validStart
	if signal > validLen {
		// 有效数据不足以计算 DEA
		return MACDResult{DIF: dif, DEA: dea, Histogram: hist}
	}

	validDIF := make([]float64, validLen)
	copy(validDIF, dif[validStart:])

	deaValid := EMA(validDIF, signal)
	for i := 0; i < validLen; i++ {
		dea[validStart+i] = deaValid[i]
	}

	// Histogram = (DIF - DEA) * 2, 仅在 DEA 有效时才有意义
	for i := 0; i < n; i++ {
		if isNaN(dea[i]) {
			continue
		}
		hist[i] = (dif[i] - dea[i]) * 2.0
	}

	return MACDResult{DIF: dif, DEA: dea, Histogram: hist}
}
