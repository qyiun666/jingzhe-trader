package model

import "testing"

// TestLimitStatusEncoding 锁定 limit_status 的真实编码
// 曾因误以为「1涨停/-1跌停/0正常」而在选股器写出 `!= 0` 过滤, 把全市场候选砍到 0 且连续 5 天无告警
func TestLimitStatusEncoding(t *testing.T) {
	cases := []struct {
		status      int
		wantLimitUp bool
		wantDown    bool
	}{
		{LimitFlat, false, false},   // 平盘
		{LimitUp, false, false},     // 上涨: 实测平均 +1.81%, 不是涨停
		{LimitDown, false, false},   // 下跌: 实测平均 -1.57%, 不是跌停
		{LimitHitUp, true, false},   // 涨停: 实测恒为 +10%
		{LimitOneWord, true, false}, // 一字/曾涨停
		{LimitHitDown, false, true}, // 跌停: 实测恒为 -10%
	}
	for _, c := range cases {
		if got := IsLimitUp(c.status); got != c.wantLimitUp {
			t.Errorf("IsLimitUp(%d) = %v, 期望 %v", c.status, got, c.wantLimitUp)
		}
		if got := IsLimitDown(c.status); got != c.wantDown {
			t.Errorf("IsLimitDown(%d) = %v, 期望 %v", c.status, got, c.wantDown)
		}
	}
}

// TestLimitStatusNeverBlocksEveryStock 涨跌停过滤是「剔除少数极值」而非「只留平盘股」
func TestLimitStatusNeverBlocksEveryStock(t *testing.T) {
	normal := []int{LimitFlat, LimitUp, LimitDown}
	for _, s := range normal {
		if IsLimitUp(s) || IsLimitDown(s) {
			t.Errorf("常规状态 %d 不应被涨跌停过滤剔除", s)
		}
	}
}
