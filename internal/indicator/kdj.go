package indicator

// KDJResult KDJ 随机指标计算结果
type KDJResult struct {
	K []float64
	D []float64
	J []float64
}

// KDJ 随机指标
// 需要 high, low, close 三个序列, 三者长度必须一致
// 默认参数: period=9
// RSV = (close - lowest_low) / (highest_high - lowest_low) * 100
// K = 2/3 * K_prev + 1/3 * RSV  (初始 K=50)
// D = 2/3 * D_prev + 1/3 * K    (初始 D=50)
// J = 3*K - 2*D
// 三个切片长度均与输入相同, 前 period-1 个为 NaN
func KDJ(highs, lows, closes []float64, period int) KDJResult {
	n := len(closes)
	k := nanSlice(n)
	d := nanSlice(n)
	j := nanSlice(n)

	// 输入校验: 三序列长度一致且非空, period 合法
	if n == 0 || len(highs) != n || len(lows) != n || period < 1 || period > n {
		return KDJResult{K: k, D: d, J: j}
	}

	// 初始 K, D 均为 50
	prevK := 50.0
	prevD := 50.0

	// RSV 从 index = period-1 开始有效
	for i := period - 1; i < n; i++ {
		highestHigh := Max(highs[i-period+1 : i+1])
		lowestLow := Min(lows[i-period+1 : i+1])

		// 计算 RSV, 除零保护: 当最高=最低时 RSV 取 50 (中性)
		rsv := 50.0
		rangeVal := highestHigh - lowestLow
		if rangeVal != 0 {
			rsv = (closes[i] - lowestLow) / rangeVal * 100.0
		}

		// K = 2/3 * K_prev + 1/3 * RSV
		prevK = 2.0/3.0*prevK + 1.0/3.0*rsv
		k[i] = prevK

		// D = 2/3 * D_prev + 1/3 * K
		prevD = 2.0/3.0*prevD + 1.0/3.0*prevK
		d[i] = prevD

		// J = 3*K - 2*D
		j[i] = 3.0*prevK - 2.0*prevD
	}

	return KDJResult{K: k, D: d, J: j}
}
