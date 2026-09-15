package market

import (
	"testing"
	"time"
)

// TestNextTradeDay 下一交易日。
func TestNextTradeDay(t *testing.T) {
	days := []string{"20260104", "20260105", "20260106", "20260107", "20260108"} // 周一~周五
	next, ok := NextTradeDay(days, "20260106")
	if !ok || next != "20260107" {
		t.Errorf("NextTradeDay(0106) = %s,%v 期望 20260107", next, ok)
	}
	// 末日后无下一交易日
	if _, ok := NextTradeDay(days, "20260108"); ok {
		t.Error("末日后不应有下一交易日")
	}
}

// TestValidUntil EOD 有效期 = 下一交易日 15:00；算不出下一交易日必须报错而不是退回自然日 +1。
func TestValidUntil(t *testing.T) {
	days := []string{"20260104", "20260105", "20260106", "20260107", "20260108"}
	// EOD 生成日 0106（周二）→ 下一交易日 0107 15:00
	eu, err := EODValidUntil(days, "20260106")
	if err != nil {
		t.Fatalf("EODValidUntil(0106) 报错: %v", err)
	}
	want := time.Date(2026, 1, 7, 15, 0, 0, 0, Loc)
	if !eu.Equal(want) {
		t.Errorf("EODValidUntil(0106) = %v, 期望 %v", eu, want)
	}
	// 日历里最后一个交易日之后没有下一交易日：报错，不给"自然日 +1"的周六有效期
	if _, err := EODValidUntil(days, "20260108"); err == nil {
		t.Error("日历无后续时应报错，实际静默给出了有效期")
	}
	// 非法日期
	if _, err := EODValidUntil([]string{"2026年1月7日"}, "20260106"); err == nil {
		t.Error("下一交易日日期非法时应报错")
	}
}

// TestIsTradeDay 日历缺失兜底为空跑。
func TestIsTradeDay(t *testing.T) {
	cal := map[string]bool{"20260104": true, "20260105": false}
	if !IsTradeDay(cal, "20990101") { // 缺失 → 空跑
		t.Error("日历缺失应返回 true（宁可空跑）")
	}
	if !IsTradeDay(cal, "20260104") {
		t.Error("交易日应返回 true")
	}
	if IsTradeDay(cal, "20260105") {
		t.Error("非交易日应返回 false")
	}
}
