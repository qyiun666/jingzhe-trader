package agent

import "testing"

func TestEvaluateDecision(t *testing.T) {
	cases := []struct {
		decision string
		retPct   float64
		want     int
	}{
		{"buy", 1.2, 1},     // 买入后涨 → 对
		{"buy", -0.5, 0},    // 买入后跌 → 错
		{"buy", 0, 0},       // 平盘 → 错 (方向未兑现)
		{"sell", -2.0, 1},   // 卖出后跌 → 对
		{"sell", 1.0, 0},    // 卖出后涨(踏空) → 错
		{"reject", 0.1, 0},  // 否决后微涨 → 错
		{"reject", -0.1, 1}, // 否决后跌 → 对
	}
	for _, c := range cases {
		if got := evaluateDecision(c.decision, c.retPct); got != c.want {
			t.Errorf("evaluateDecision(%s, %.2f) = %d, want %d", c.decision, c.retPct, got, c.want)
		}
	}
}
