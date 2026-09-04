package market

import (
	"testing"
	"time"
)

// TestQuarterOf 季度边界正确（验收 #8）。
func TestQuarterOf(t *testing.T) {
	cases := []struct {
		date      string
		wantLabel string
		wantStart string
		wantEnd   string
	}{
		{"20260101", "2026Q1", "2026-01-01", "2026-03-31"},
		{"20260331", "2026Q1", "2026-01-01", "2026-03-31"},
		{"20260401", "2026Q2", "2026-04-01", "2026-06-30"},
		{"20260930", "2026Q3", "2026-07-01", "2026-09-30"},
		{"20261001", "2026Q4", "2026-10-01", "2026-12-31"},
		{"20261231", "2026Q4", "2026-10-01", "2026-12-31"},
	}
	for _, c := range cases {
		label, start, end := QuarterOf(c.date)
		if label != c.wantLabel || start != c.wantStart || end != c.wantEnd {
			t.Errorf("QuarterOf(%s) = (%s,%s,%s), 期望 (%s,%s,%s)",
				c.date, label, start, end, c.wantLabel, c.wantStart, c.wantEnd)
		}
	}
}

// TestQuarterTradeDays 季度交易日进度（自然日边界 + 跨年）。
func TestQuarterTradeDays(t *testing.T) {
	// 构造 2026Q1 的交易日（跳过周末，简单列举部分）
	days := []string{
		"20260102", "20260105", "20260106", "20260107", "20260108", "20260109",
		"20260330", "20260331",
	}
	elapsed, total := QuarterTradeDays(days, "20260107")
	if total != 8 {
		t.Errorf("Q1 总交易日应 8，实际 %d", total)
	}
	if elapsed != 4 { // 截至 0107（含）有 0102,0105,0106,0107 共 4 个
		t.Errorf("截至 0107 已过交易日应 4，实际 %d", elapsed)
	}
	// 跨年：2026-12-31 属于 Q4
	_, totalQ4 := QuarterTradeDays([]string{"20261230", "20261231"}, "20261231")
	if totalQ4 != 2 {
		t.Errorf("Q4 总交易日应 2，实际 %d", totalQ4)
	}
}

// TestNextPrevTradeDay 上一/下一交易日。
func TestNextPrevTradeDay(t *testing.T) {
	days := []string{"20260104", "20260105", "20260106", "20260107", "20260108"} // 周一~周五
	next, ok := NextTradeDay(days, "20260106")
	if !ok || next != "20260107" {
		t.Errorf("NextTradeDay(0106) = %s,%v 期望 20260107", next, ok)
	}
	prev, ok := PrevTradeDay(days, "20260106")
	if !ok || prev != "20260105" {
		t.Errorf("PrevTradeDay(0106) = %s,%v 期望 20260105", prev, ok)
	}
	// 首日前无上一交易日
	if _, ok := PrevTradeDay(days, "20260104"); ok {
		t.Error("首日前不应有上一交易日")
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
