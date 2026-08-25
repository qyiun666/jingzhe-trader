package indicator

// go-talib 适配层
// 项目指标契约: 输出等长 + 无效位置 NaN; go-talib 输出等长但无效位置为 0
// 委托前提: talib 算法与自研完全一致 (由 indicator_test.go 黄金测试守护)
// 数值对齐验证结论 (随机+边界数据):
//   - SMA/WMA/BOLL 可委托
//   - RSI 横盘边界 (gain=loss=0 时 talib=0 vs 自研=50) 与 ATR 首值窗口偏移, 保留自研
//   - EMA/MACD/KDJ 初始化差异, 保留自研

import (
	"math"
)

// padTo 将 go-talib 等长输出 (无效位置为 0) 适配为项目契约 (无效位置为 NaN)
// startIdx: 首个有效值在输入序列中的位置 (SMA/WMA/BOLL/CCI 为 period-1, RSI/ROC/ADX 为 period)
func padTo(values []float64, n, startIdx int) []float64 {
	out := nanSlice(n)
	for i := startIdx; i < n; i++ {
		out[i] = values[i]
	}
	return out
}

// hasNaN 判断切片是否含 NaN
// talib 对 NaN 的传播与自研不同 (零值填充), 含 NaN 的输入一律回退自研
func hasNaN(values []float64) bool {
	for _, v := range values {
		if math.IsNaN(v) {
			return true
		}
	}
	return false
}
