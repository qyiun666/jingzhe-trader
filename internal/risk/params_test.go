package risk

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// resolveCase 单例期望（逐字段断言，验收 #6）。
type resolveCase struct {
	name             string
	gear             model.Gear
	lock             bool
	pace             PaceAdjust
	wantTotal        float64
	wantSingle       float64
	wantStop         float64
	wantTrail        float64
	wantMaxPos       int
	wantConf         float64
	wantNewPos       bool
	wantBias         StrategyBias
	wantSingleAmtPct float64
}

// base 总资产 1 万元（自适应持仓数 = 2）。
func base() RiskParams {
	return DefaultBase(model.Fen(10000 * 100))
}

// TestResolveTableDriven 对应验收 #6：G1/G2/G3 × 锁利 × 落后策略 ≥20 例，逐字段断言。
func TestResolveTableDriven(t *testing.T) {
	u := func(gap, budget float64) PaceAdjust { return UnrestrictedPace{PaceGap: gap, BudgetConsumed: budget} }
	c := func(gap float64) PaceAdjust { return ConservativePace{PaceGap: gap} }

	cases := []resolveCase{
		// ---------- 无锁利 × 无落后策略（3 档全量） ----------
		{"G1基准", model.GearG1, false, NoPace{}, 0.90, 0.40, 0.08, 0.05, 2, 0.55, true, BiasDynamic, 1.0},
		{"G2基准", model.GearG2, false, NoPace{}, 0.60, 0.25, 0.06, 0.04, 2, 0.70, true, BiasTrendFilter, 0.5},
		{"G3基准", model.GearG3, false, NoPace{}, 0.30, 0.15, 0.05, 0.03, 2, 1.0, false, BiasExitOnly, 0.0},
		// ---------- 锁利叠加（3 档） ----------
		{"G1锁利", model.GearG1, true, NoPace{}, 0.63, 0.40, 0.05, 0.05, 2, 0.55, false, BiasDynamic, 1.0},
		{"G2锁利", model.GearG2, true, NoPace{}, 0.42, 0.25, 0.05, 0.04, 2, 0.70, false, BiasTrendFilter, 0.5},
		{"G3锁利", model.GearG3, true, NoPace{}, 0.21, 0.15, 0.05, 0.03, 2, 1.0, false, BiasExitOnly, 0.0},
		// ---------- UnrestrictedPace 追赶曲线（G1） ----------
		{"G1放开gap0.10", model.GearG1, false, u(0.10, 0), 0.90, 0.40, 0.08, 0.05, 2, 0.55, true, BiasDynamic, 1.0},
		{"G1放开gap0.20", model.GearG1, false, u(0.20, 0), 0.95, 0.40, 0.07, 0.05, 2, 0.55, true, BiasDynamic, 1.0},
		{"G1放开gap0.45", model.GearG1, false, u(0.45, 0), 0.95, 0.40, 0.06, 0.05, 2, 0.55, true, BiasDynamic, 1.0},
		{"G1放开gap0.70", model.GearG1, false, u(0.70, 0), 0.95, 0.40, 0.05, 0.05, 2, 0.55, true, BiasDynamic, 1.0},
		// ---------- UnrestrictedPace（G2） ----------
		{"G2放开gap0.20", model.GearG2, false, u(0.20, 0), 0.69, 0.25, 0.05, 0.04, 2, 0.70, true, BiasTrendFilter, 0.5},
		{"G2放开gap0.45", model.GearG2, false, u(0.45, 0), 0.78, 0.25, 0.04, 0.04, 2, 0.70, true, BiasTrendFilter, 0.5},
		{"G2放开gap0.70", model.GearG2, false, u(0.70, 0), 0.90, 0.25, 0.04, 0.04, 2, 0.70, true, BiasTrendFilter, 0.5},
		// ---------- UnrestrictedPace（G3：本就禁新仓，加成不改变开关） ----------
		{"G3放开gap0.45", model.GearG3, false, u(0.45, 0), 0.39, 0.15, 0.04, 0.03, 2, 1.0, false, BiasExitOnly, 0.0},
		// ---------- 回撤预算耗尽 → 强制防守 ----------
		{"G1预算耗尽强制G3", model.GearG1, false, u(0.70, 1.00), 0.30, 0.15, 0.05, 0.03, 2, 1.0, false, BiasExitOnly, 0.0},
		{"G2预算耗尽强制G3", model.GearG2, true, u(0.20, 1.00), 0.30, 0.15, 0.05, 0.03, 2, 1.0, false, BiasExitOnly, 0.0},
		// ---------- ConservativePace 只动门槛、永不动仓位/止损 ----------
		{"G1保守gap0.10", model.GearG1, false, c(0.10), 0.90, 0.40, 0.08, 0.05, 2, 0.55, true, BiasDynamic, 1.0},
		{"G1保守gap0.20", model.GearG1, false, c(0.20), 0.90, 0.40, 0.08, 0.05, 2, 0.60, true, BiasDynamic, 1.0},
		{"G1保守gap0.35", model.GearG1, false, c(0.35), 0.90, 0.40, 0.08, 0.05, 2, 0.65, true, BiasDynamic, 1.0},
		{"G2保守gap0.35", model.GearG2, false, c(0.35), 0.60, 0.25, 0.06, 0.04, 2, 0.70, true, BiasTrendFilter, 0.5},
		{"G1保守+锁利", model.GearG1, true, c(0.20), 0.63, 0.40, 0.05, 0.05, 2, 0.60, false, BiasDynamic, 1.0},
		// ---------- G3 已是防守档：落后策略不再放宽任何口径 ----------
		{"G3保守gap0.35", model.GearG3, false, c(0.35), 0.30, 0.15, 0.05, 0.03, 2, 1.0, false, BiasExitOnly, 0.0},
	}

	if len(cases) < 20 {
		t.Fatalf("表驱动用例不足 20 例: %d", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mustResolve(base(), tc.gear, tc.lock, tc.pace)
			assertEqF(t, tc.name, "MaxTotalPositionPct", got.MaxTotalPositionPct, tc.wantTotal)
			assertEqF(t, tc.name, "MaxPositionPct", got.MaxPositionPct, tc.wantSingle)
			assertEqF(t, tc.name, "StopLossPct", got.StopLossPct, tc.wantStop)
			assertEqF(t, tc.name, "TrailingStopPct", got.TrailingStopPct, tc.wantTrail)
			if got.MaxPositions != tc.wantMaxPos {
				t.Errorf("%s: MaxPositions=%d, 期望 %d", tc.name, got.MaxPositions, tc.wantMaxPos)
			}
			assertEqF(t, tc.name, "MinConfidence", got.MinConfidence, tc.wantConf)
			if got.AllowNewPosition != tc.wantNewPos {
				t.Errorf("%s: AllowNewPosition=%v, 期望 %v", tc.name, got.AllowNewPosition, tc.wantNewPos)
			}
			if got.Bias != tc.wantBias {
				t.Errorf("%s: Bias=%s, 期望 %s", tc.name, got.Bias, tc.wantBias)
			}
			assertEqF(t, tc.name, "MaxSingleAmountPct", got.MaxSingleAmountPct, tc.wantSingleAmtPct)
		})
	}
}

// TestResolveAdaptiveMaxPositions 持仓数自适应：<5万→2，<20万→4，否则 6（G1 基准）。
func TestResolveAdaptiveMaxPositions(t *testing.T) {
	cases := []struct {
		assetYuan float64
		wantG1    int
		wantG2    int
		wantG3    int
	}{
		{9000, 2, 2, 2},   // <1万 → 2（G2 = max(2-1,2)=2）
		{49000, 2, 2, 2},  // <5万 → 2
		{50000, 4, 3, 2},  // 5万 → 4（G2 −1）
		{199000, 4, 3, 2}, // <20万 → 4
		{200000, 6, 5, 2}, // ≥20万 → 6（G2 −1）
	}
	for _, tc := range cases {
		asset := model.Fen(tc.assetYuan * 100)
		if got := mustResolve(DefaultBase(asset), model.GearG1, false, NoPace{}).MaxPositions; got != tc.wantG1 {
			t.Errorf("资产 %.0f 元 G1 MaxPositions=%d, 期望 %d", tc.assetYuan, got, tc.wantG1)
		}
		if got := mustResolve(DefaultBase(asset), model.GearG2, false, NoPace{}).MaxPositions; got != tc.wantG2 {
			t.Errorf("资产 %.0f 元 G2 MaxPositions=%d, 期望 %d", tc.assetYuan, got, tc.wantG2)
		}
		if got := mustResolve(DefaultBase(asset), model.GearG3, false, NoPace{}).MaxPositions; got != tc.wantG3 {
			t.Errorf("资产 %.0f 元 G3 MaxPositions=%d, 期望 %d", tc.assetYuan, got, tc.wantG3)
		}
	}
}

// TestResolveCircuitBreaker 物理熔断底线永远生效：总仓 ≤0.95、单票 ≤0.60 不可被任何组合突破。
func TestResolveCircuitBreaker(t *testing.T) {
	for _, gear := range []model.Gear{model.GearG1, model.GearG2, model.GearG3} {
		for _, lock := range []bool{false, true} {
			for _, gap := range []float64{0.0, 0.2, 0.45, 0.7} {
				p := mustResolve(base(), gear, lock, UnrestrictedPace{PaceGap: gap, BudgetConsumed: 0})
				if p.MaxTotalPositionPct > CircuitMaxTotalPct {
					t.Errorf("%s lock=%v gap=%.2f: 总仓位 %.2f 突破熔断上限 %.2f", gear, lock, gap, p.MaxTotalPositionPct, CircuitMaxTotalPct)
				}
				if p.MaxPositionPct > CircuitMaxSinglePct {
					t.Errorf("%s lock=%v gap=%.2f: 单票 %.2f 突破熔断上限 %.2f", gear, lock, gap, p.MaxPositionPct, CircuitMaxSinglePct)
				}
			}
		}
	}
}

// TestResolvePure 纯函数性：同一输入两次 Resolve 输出一致，且不修改 base。
func TestResolvePure(t *testing.T) {
	b := base()
	b1 := mustResolve(b, model.GearG2, true, UnrestrictedPace{PaceGap: 0.4, BudgetConsumed: 0.5})
	b2 := mustResolve(b, model.GearG2, true, UnrestrictedPace{PaceGap: 0.4, BudgetConsumed: 0.5})
	if b1 != b2 {
		t.Errorf("Resolve 非纯函数: 两次输出不一致 %+v vs %+v", b1, b2)
	}
	if b.MaxTotalPositionPct != 0 || b.MaxPositionPct != 0 {
		// base 未被写入档位值（0 为零值，说明未被污染）
		t.Errorf("Resolve 污染了 base: %+v", b)
	}
}

func assertEqF(t *testing.T, caseName, field string, got, want float64) {
	t.Helper()
	const eps = 1e-9
	if diff := got - want; diff > eps || diff < -eps {
		t.Errorf("%s: %s=%.6f, 期望 %.6f", caseName, field, got, want)
	}
}

// TestResolveUnknownGearIsError 未知档位必须报错：map 未命中的零值 gearSpec 里
// StopLossPct=0 等于「成本价即止损线」，会把每个持仓都判成止损，不是安全的兜底。
func TestResolveUnknownGearIsError(t *testing.T) {
	for _, gear := range []model.Gear{"", "G0", "G9", "g1", "G1 "} {
		p, err := Resolve(base(), gear, false, NoPace{})
		if err == nil {
			t.Errorf("gear=%q 竟然解析成功，得到 %+v（零值 StopLossPct=%.2f 会清光仓位）", gear, p, p.StopLossPct)
			continue
		}
		if !strings.Contains(err.Error(), "未知风控档位") {
			t.Errorf("gear=%q 错误信息不含档位名: %v", gear, err)
		}
	}
}

// TestResolveKnownGearsHaveStopLoss 三档生效参数都必须带非零止损/移动止盈：
// 这条断言防的是「档位加了行但漏填字段」，零值止损会静默变成全仓即时止损。
func TestResolveKnownGearsHaveStopLoss(t *testing.T) {
	for _, gear := range []model.Gear{model.GearG1, model.GearG2, model.GearG3} {
		p := mustResolve(base(), gear, false, NoPace{})
		if p.StopLossPct <= 0 || p.StopLossPct >= 1 {
			t.Errorf("%s StopLossPct=%.3f 不在 (0,1)", gear, p.StopLossPct)
		}
		if p.TrailingStopPct <= 0 || p.TrailingStopPct >= 1 {
			t.Errorf("%s TrailingStopPct=%.3f 不在 (0,1)", gear, p.TrailingStopPct)
		}
		if p.MaxPositions <= 0 {
			t.Errorf("%s MaxPositions=%d 必须为正", gear, p.MaxPositions)
		}
	}
}
