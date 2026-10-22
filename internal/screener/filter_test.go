package screener

import "testing"

// TestListDaysBetween 上市天数必须按真实自然日算：跨月/跨年借位、闰年、
// 倒序与坏格式都要走对。旧实现按 YYYYMMDD 整数差做 y*365+m*30+d，
// 借位时月份字段会被吃掉（20260125→20260210 曾算成 85 天），
// 60 日新股门槛因此可能被绕过。
func TestListDaysBetween(t *testing.T) {
	cases := []struct {
		name, list, trade string
		want              int
	}{
		{"同日", "20260125", "20260125", 0},
		{"跨月借位", "20260125", "20260210", 16},
		{"跨年借位", "20251220", "20260301", 71},
		{"闰年二月", "20240228", "20240301", 2},
		{"闰日当天", "20240229", "20240301", 1},
		{"年底跨年", "20251231", "20260101", 1},
		{"倒序（坏数据）", "20260301", "20260210", -1},
		{"带分隔符", "2026-01-25", "20260210", -1},
		{"非法日期", "20260230", "20260301", -1},
	}
	for _, c := range cases {
		if got := listDaysBetween(c.list, c.trade); got != c.want {
			t.Errorf("%s：listDaysBetween(%q→%q)=%d，期望 %d", c.name, c.list, c.trade, got, c.want)
		}
	}
}
