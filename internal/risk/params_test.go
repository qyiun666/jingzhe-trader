package risk

import (
	"testing"

	"jingzhe-trader/internal/model"
)

// baseAsset 总资产 1 万元（自适应持仓数 = 2）。
func baseAsset() model.Fen { return model.Fen(10000 * 100) }

// TestDefaultParamsBaseline 固定基准逐字段断言：删除档位状态机后，
// 生效参数不再随季度进度浮动，这一组值是唯一的仓位/止损口径。
// 1 万元资产的持仓数自适应为 2（见 TestDefaultParamsAdaptiveMaxPositions）。
func TestDefaultParamsBaseline(t *testing.T) {
	p := DefaultParams(baseAsset())
	assertEqF(t, "基准", "MaxTotalPositionPct", p.MaxTotalPositionPct, 0.90)
	assertEqF(t, "基准", "MaxPositionPct", p.MaxPositionPct, 0.40)
	assertEqF(t, "基准", "StopLossPct", p.StopLossPct, 0.08)
	assertEqF(t, "基准", "TrailingStopPct", p.TrailingStopPct, 0.05)
	assertEqF(t, "基准", "TakeProfitPct", p.TakeProfitPct, 0.15)
	assertEqF(t, "基准", "MinConfidence", p.MinConfidence, 0.55)
	assertEqF(t, "基准", "MaxSingleAmountPct", p.MaxSingleAmountPct, 1.0)
	if p.MaxPositions != 2 {
		t.Errorf("MaxPositions=%d, 期望 2", p.MaxPositions)
	}
	if !p.AllowNewPosition {
		t.Errorf("AllowNewPosition=%v, 期望 true", p.AllowNewPosition)
	}
}

// TestDefaultParamsAdaptiveMaxPositions 持仓数自适应：<5万→2，<20万→4，否则 6。
func TestDefaultParamsAdaptiveMaxPositions(t *testing.T) {
	cases := []struct {
		assetYuan float64
		want      int
	}{
		{9000, 2},
		{49000, 2},
		{50000, 4},
		{199000, 4},
		{200000, 6},
	}
	for _, tc := range cases {
		got := DefaultParams(model.Fen(tc.assetYuan * 100)).MaxPositions
		if got != tc.want {
			t.Errorf("资产 %.0f 元 MaxPositions=%d, 期望 %d", tc.assetYuan, got, tc.want)
		}
	}
}

// TestDefaultParamsCircuitBreaker 物理熔断底线永远生效：总仓 ≤0.95、单票 ≤0.60。
func TestDefaultParamsCircuitBreaker(t *testing.T) {
	for _, yuan := range []float64{5000, 10000, 50000, 200000, 1000000} {
		p := DefaultParams(model.Fen(yuan * 100))
		if p.MaxTotalPositionPct > CircuitMaxTotalPct {
			t.Errorf("资产 %.0f: 总仓位 %.2f 突破熔断上限 %.2f", yuan, p.MaxTotalPositionPct, CircuitMaxTotalPct)
		}
		if p.MaxPositionPct > CircuitMaxSinglePct {
			t.Errorf("资产 %.0f: 单票 %.2f 突破熔断上限 %.2f", yuan, p.MaxPositionPct, CircuitMaxSinglePct)
		}
	}
}

// TestDefaultParamsPure 纯函数性：同一输入两次调用输出一致，且不修改入参。
func TestDefaultParamsPure(t *testing.T) {
	a := baseAsset()
	p1 := DefaultParams(a)
	p2 := DefaultParams(a)
	if p1 != p2 {
		t.Errorf("DefaultParams 非纯函数: 两次输出不一致 %+v vs %+v", p1, p2)
	}
	if a != baseAsset() {
		t.Errorf("入参被污染: %v", int64(a))
	}
}

// TestDefaultParamsStopLossNonZero 生效参数必须带非零止损/移动止盈：
// 零值止损会静默变成全仓即时止损，这条断言防的是"默认值漏填字段"。
func TestDefaultParamsStopLossNonZero(t *testing.T) {
	p := DefaultParams(baseAsset())
	if p.StopLossPct <= 0 || p.StopLossPct >= 1 {
		t.Errorf("StopLossPct=%.3f 不在 (0,1)", p.StopLossPct)
	}
	if p.TrailingStopPct <= 0 || p.TrailingStopPct >= 1 {
		t.Errorf("TrailingStopPct=%.3f 不在 (0,1)", p.TrailingStopPct)
	}
	if p.MaxPositions <= 0 {
		t.Errorf("MaxPositions=%d 必须为正", p.MaxPositions)
	}
}

func assertEqF(t *testing.T, caseName, field string, got, want float64) {
	t.Helper()
	const eps = 1e-9
	if diff := got - want; diff > eps || diff < -eps {
		t.Errorf("%s: %s=%.6f, 期望 %.6f", caseName, field, got, want)
	}
}
