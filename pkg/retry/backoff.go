// Package retry 提供通用的指数退避计算工具
package retry

import "time"

// Backoff 返回第 attempt 次重试前的等待时长
// attempt 从 1 开始计数: 第1次重试等 base, 第2次等 2*base, 第3次等 4*base
func Backoff(base time.Duration, attempt int) time.Duration {
	return base << (attempt - 1)
}
