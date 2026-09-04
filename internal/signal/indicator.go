// Package signal 决策编排：买入决策交给 LLM 评审（本包只提供证据列与接口），
// 卖出按规则执行；结果落成 order_ticket，信号本身不落库（ARCHITECTURE §2.8）。
//
// 依赖方向：signal 依赖 model / market / risk / store / ticket（编排回执前的最后一级），
// 不触网；行情与持仓数据由 store 提供。
package signal

import "math"

// SMA 简单移动平均（取末尾 n 个；不足 n 时用全部可用数据）。
func SMA(vals []float64, n int) float64 {
	if n <= 0 || len(vals) == 0 {
		return 0
	}
	if n > len(vals) {
		n = len(vals)
	}
	sum := 0.0
	for _, v := range vals[len(vals)-n:] {
		sum += v
	}
	return sum / float64(n)
}

// VolumeRatio 量比：最新成交量 ÷ 前 5 期均量（不足 5 期用可用期数；无历史返回 0）。
func VolumeRatio(vols []float64) float64 {
	if len(vols) < 2 {
		return 0
	}
	last := vols[len(vols)-1]
	prev := vols[len(vols)-1-5:]
	if len(prev) > 5 {
		prev = prev[len(prev)-5:]
	}
	// prev 含最后一期，去掉
	prev = prev[:len(prev)-1]
	if len(prev) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range prev {
		sum += v
	}
	avg := sum / float64(len(prev))
	if avg <= 0 {
		return 0
	}
	return last / avg
}

// MA5MA20 返回 (MA5, MA20)；数据不足 20 根时 ma20=NaN（调用方据此判不通过）。
func MA5MA20(closes []float64) (ma5, ma20 float64) {
	if len(closes) < 20 {
		return math.NaN(), math.NaN()
	}
	return SMA(closes, 5), SMA(closes, 20)
}
