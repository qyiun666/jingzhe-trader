package goal

import (
	"math"
	"testing"

	"jingzhe-trader/internal/risk"
)

// ---------- 落后策略：激进模式三重保护（§10.5-3）----------

func TestAggressiveDenied(t *testing.T) {
	s := PaceSettings{Policy: PolicyAggressive, MaxBoostPct: 0.10, BudgetBelow: 0.30}

	cases := []struct {
		name     string
		m        GoalMetrics
		confirm  string
		today    string
		wantOK   bool
		wantCode string
	}{
		{"allowed_when_confirmed_and_budget_ok", GoalMetrics{PaceGap: 0.20, BudgetConsumed: 0.10}, "20260102", "20260102", false, ""},
		{"denied_budget_too_high", GoalMetrics{PaceGap: 0.20, BudgetConsumed: 0.40}, "20260102", "20260102", true, "PACE_BOOST_DENIED"},
		{"denied_no_confirm", GoalMetrics{PaceGap: 0.20, BudgetConsumed: 0.10}, "", "20260102", true, "PACE_BOOST_EXPIRED"},
		{"denied_confirm_stale", GoalMetrics{PaceGap: 0.20, BudgetConsumed: 0.10}, "20260101", "20260102", true, "PACE_BOOST_EXPIRED"},
		{"not_denied_when_not_behind", GoalMetrics{PaceGap: 0.10, BudgetConsumed: 0.10}, "20260102", "20260102", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, ok := AggressiveDenied(s, tc.m, tc.confirm, tc.today)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}

	// 非激进策略不应被拒
	if code, _, ok := AggressiveDenied(PaceSettings{Policy: PolicyUnrestricted}, GoalMetrics{PaceGap: 0.20, BudgetConsumed: 0.40}, "", "20260102"); ok {
		t.Errorf("unrestricted 不应被拒，却返回 code=%q", code)
	}
}

func TestAggressivePaceApply(t *testing.T) {
	approx := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	base := risk.RiskParams{MaxTotalPositionPct: 0.90}

	cases := []struct {
		name string
		pace AggressivePace
		want float64
	}{
		{"boost_5pct", AggressivePace{PaceGap: 0.20, BudgetConsumed: 0.10, MaxBoostPct: 0.05, BudgetBelow: 0.30, Confirmed: true}, 0.90 * 1.05},
		{"boost_capped_to_10pct", AggressivePace{PaceGap: 0.20, BudgetConsumed: 0.10, MaxBoostPct: 0.20, BudgetBelow: 0.30, Confirmed: true}, 0.90 * 1.10},
		{"no_boost_when_not_behind", AggressivePace{PaceGap: 0.10, BudgetConsumed: 0.10, MaxBoostPct: 0.05, BudgetBelow: 0.30, Confirmed: true}, 0.90},
		{"no_boost_when_budget_high", AggressivePace{PaceGap: 0.20, BudgetConsumed: 0.50, MaxBoostPct: 0.05, BudgetBelow: 0.30, Confirmed: true}, 0.90},
		{"no_boost_when_unconfirmed", AggressivePace{PaceGap: 0.20, BudgetConsumed: 0.10, MaxBoostPct: 0.05, BudgetBelow: 0.30, Confirmed: false}, 0.90},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.pace.Apply(base)
			if !approx(got.MaxTotalPositionPct, tc.want) {
				t.Errorf("MaxTotalPositionPct = %v, want %v", got.MaxTotalPositionPct, tc.want)
			}
		})
	}
}

func TestNewPaceAdjust(t *testing.T) {
	m := GoalMetrics{PaceGap: 0.20, BudgetConsumed: 0.10}
	// aggressive 装配出的 PaceAdjust 应为 AggressivePace 且 Confirmed 根据日期判定
	agg := NewPaceAdjust(PaceSettings{Policy: PolicyAggressive, MaxBoostPct: 0.10, BudgetBelow: 0.30}, m, "20260102", "20260102")
	if _, ok := agg.(AggressivePace); !ok {
		t.Errorf("aggressive 应装配为 AggressivePace，实际 %T", agg)
	}
	// unrestricted -> UnrestrictedPace
	unr := NewPaceAdjust(PaceSettings{Policy: PolicyUnrestricted}, m, "", "20260102")
	if _, ok := unr.(risk.UnrestrictedPace); !ok {
		t.Errorf("unrestricted 应装配为 UnrestrictedPace，实际 %T", unr)
	}
}
