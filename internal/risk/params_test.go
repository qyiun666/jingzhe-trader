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
	// 0.06 → 0.08（20261103 定参②）：arm=6% 配 d=5% 的触发价下限只有成本×1.007，
	// 而小单双边摩擦实测 1.09%~1.78% —— 止盈在武装线附近触发即注定净亏。
	// 静态下限与 config risk.trailing_arm_pct 的 KeySpec 默认值必须一致，
	// 两处不一致时线上取 config（deps.go 覆盖基准），但读代码的人会被基准值误导。
	assertEqF(t, "基准", "TrailingArmPct", p.TrailingArmPct, 0.08)
	assertEqF(t, "基准", "TrailingArmSafetyPct", p.TrailingArmSafetyPct, 0.01)
	assertEqF(t, "基准", "ExpectedExcessPct", p.ExpectedExcessPct, 0)
	assertEqF(t, "基准", "TakeProfitPct", p.TakeProfitPct, 0.15)
	assertEqF(t, "基准", "MinConfidence", p.MinConfidence, 0.55)
	assertEqF(t, "基准", "MaxSingleAmountPct", p.MaxSingleAmountPct, 1.0)
	assertEqF(t, "基准", "MinAmountRatio", p.MinAmountRatio, 0.5)
	if p.MaxPositions != 2 {
		t.Errorf("MaxPositions=%d, 期望 2", p.MaxPositions)
	}
	if p.MaxPerIndustry != 2 {
		t.Errorf("MaxPerIndustry=%d, 期望 2", p.MaxPerIndustry)
	}
	if !p.AllowNewPosition {
		t.Errorf("AllowNewPosition=%v, 期望 true", p.AllowNewPosition)
	}
}

// TestBuyIndustryCap 买入侧行业上限由候选侧配置推导：0=关闭，1 跟随收紧，
// 2 及以上封顶 2（账户只有 2 个持仓名额，同行业 3 只没有意义）。
func TestBuyIndustryCap(t *testing.T) {
	cases := map[int]int{0: 0, 1: 1, 2: 2, 3: 2, 10: 2}
	for in, want := range cases {
		if got := BuyIndustryCap(in); got != want {
			t.Errorf("BuyIndustryCap(%d)=%d，期望 %d", in, got, want)
		}
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
	if p.TrailingArmPct < 0 || p.TrailingArmPct > 0.5 {
		t.Errorf("TrailingArmPct=%.3f 不在 [0,0.5]", p.TrailingArmPct)
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

// TestWeakParamsShrinksOnly 弱势试探收缩：只允许把仓位上限压低，任何输入都
// 不能把单票/总仓抬到高于正常基准或物理熔断；坏值回落默认档而不是放行。
func TestWeakParamsShrinksOnly(t *testing.T) {
	base := DefaultParams(baseAsset()) // 单票 0.40 / 总仓 0.90

	// 正常收缩：20%/10%
	p := WeakParams(base, 0.20, 0.10)
	assertEqF(t, "收缩", "MaxTotalPositionPct", p.MaxTotalPositionPct, 0.20)
	assertEqF(t, "收缩", "MaxPositionPct", p.MaxPositionPct, 0.10)

	// 上限配得比正常档还宽：钳到不超过入参，且不超过熔断
	p = WeakParams(base, 0.99, 0.99)
	if p.MaxTotalPositionPct > CircuitMaxTotalPct {
		t.Errorf("总仓 %v 越物理熔断 %v", p.MaxTotalPositionPct, CircuitMaxTotalPct)
	}
	if p.MaxPositionPct > CircuitMaxSinglePct {
		t.Errorf("单票 %v 越物理熔断 %v", p.MaxPositionPct, CircuitMaxSinglePct)
	}
	if p.MaxPositionPct > p.MaxTotalPositionPct {
		t.Errorf("单票 %v 不应大于总仓 %v", p.MaxPositionPct, p.MaxTotalPositionPct)
	}

	// 非正/坏值回落弱势默认档
	p = WeakParams(base, 0, -1)
	assertEqF(t, "坏值回落", "MaxTotalPositionPct", p.MaxTotalPositionPct, WeakMaxTotalPctDefault)
	assertEqF(t, "坏值回落", "MaxPositionPct", p.MaxPositionPct, WeakMaxSinglePctDefault)

	// 只动仓位口径：止损/止盈/持仓数/置信度原样保留；单笔下限比例按弱势档提高
	p = WeakParams(base, 0.20, 0.10)
	if p.StopLossPct != base.StopLossPct || p.TakeProfitPct != base.TakeProfitPct ||
		p.TrailingArmPct != base.TrailingArmPct ||
		p.MaxPositions != base.MaxPositions || p.MinConfidence != base.MinConfidence {
		t.Errorf("WeakParams 不应改动仓位以外的风控字段")
	}
	assertEqF(t, "弱势", "MinAmountRatio", p.MinAmountRatio, WeakMinAmountRatio)
	if p.MaxPerIndustry != 1 {
		t.Errorf("弱势档同行业上限=%d，期望 1", p.MaxPerIndustry)
	}
	// 显式关闭行业上限（0）时弱势档不擅自打开。
	off := base
	off.MaxPerIndustry = 0
	if got := WeakParams(off, 0.20, 0.10).MaxPerIndustry; got != 0 {
		t.Errorf("行业上限显式关闭时弱势档仍应为 0，实际 %d", got)
	}
}

// TestWeakParamsRaisesMinAmountFloor 弱势档的最小交易额必须高于旧口径（单票上限的一半）：
// 单票约 1000 元时，500 元的单子双边成本约 10 元（2%），750 元起才降到约 1.4%。
func TestWeakParamsRaisesMinAmountFloor(t *testing.T) {
	weak := WeakParams(DefaultParams(baseAsset()), 0.20, 0.10) // 单票 1000 元
	oldFloor := weak.SingleCapFen() / 2
	got := weak.MinAmountFloor()
	if got <= oldFloor {
		t.Errorf("弱势档单笔下限=%s，应高于旧口径 %s", got, oldFloor)
	}
	if want := pctOf(weak.SingleCapFen(), WeakMinAmountRatio); got != want {
		t.Errorf("弱势档单笔下限=%s，期望 单票上限×%.2f=%s", got, WeakMinAmountRatio, want)
	}
}

// TestWeakParamsBlocksBuysByDefault 弱势试探期买入闸（20261103 定参①）：
// 判到弱势 + 键没配（默认 false）= 一律不下发买单；显式回退才放行。
// 断言"默认即拦截"很关键——线上库里没有这个键，生效值全靠这里的零值方向。
func TestWeakParamsBlocksBuysByDefault(t *testing.T) {
	base := DefaultParams(baseAsset())
	if base.WeakBuyAllowed { // 默认 false（新政策），不是漏赋值的 true
		t.Fatalf("DefaultParams.WeakBuyAllowed=%v，期望 false", base.WeakBuyAllowed)
	}
	if base.BuysBlocked {
		t.Errorf("非弱势档不应置 BuysBlocked")
	}
	weak := WeakParams(base, 0.20, 0.10)
	if !weak.WeakProbe || !weak.BuysBlocked {
		t.Errorf("弱势档默认应同时置 WeakProbe 与 BuysBlocked: %+v", weak)
	}
	// 回退开关：只放开买入，不改变弱势档的其他任何收缩（仓位/行业/下限）。
	base.WeakBuyAllowed = true
	rollback := WeakParams(base, 0.20, 0.10)
	if rollback.BuysBlocked {
		t.Errorf("signal.weak_buy_enabled=true 应回退买入闸，实际仍被拦")
	}
	if rollback.MaxPositionPct != weak.MaxPositionPct || rollback.MinAmountRatio != weak.MinAmountRatio ||
		rollback.MaxPerIndustry != weak.MaxPerIndustry || !rollback.WeakProbe {
		t.Errorf("回退买入闸不应连带放松弱势档其他收缩")
	}
}

// TestFrictionPctMatchesRealFills 摩擦率估算必须贴住本轮 4 笔券商实际回执：
// 5.95×100→1.78%、6.39×100→1.67%、8.21×100→1.32%、5.05×200→1.09%
// （费用合计 10.61/10.64/10.83/10.98 元，见 logs/jingzhe-new.log 成交回执）。
// 这条是摩擦闸与动态武装线共同的分母，估歪就等于闸形同虚设或误拦。
func TestFrictionPctMatchesRealFills(t *testing.T) {
	cases := []struct {
		notionalYuan float64
		wantPct      float64
	}{
		{595, 1.78}, {639, 1.67}, {821, 1.32}, {1010, 1.09},
	}
	for _, tc := range cases {
		got := FrictionPct(model.FromFloat(tc.notionalYuan)) * 100
		if diff := got - tc.wantPct; diff > 0.03 || diff < -0.03 {
			t.Errorf("%.0f 元摩擦率=%.3f%%，期望≈%.2f%%", tc.notionalYuan, got, tc.wantPct)
		}
	}
	if got := FrictionPct(0); got != 1 {
		t.Errorf("非正名义额应按最保守的 100%% 处理，实际 %.4f", got)
	}
}

// TestTrailingArmPctForCoversFriction 定参②的算术不变量：
// 触发价下限 = 成本×(1+arm)×(1−d) 必须高于 成本×(1+摩擦)，即触发后净毛利为正。
//
// 同时锁住旧参数为什么是错的：arm=6% 配 d=5% 时触发价只到成本×1.007（+0.70% 毛利），
// 而 595 元那笔摩擦 1.78% → 净 -1.08%，止盈一触发就必亏，与标的涨跌无关。
// 这笔就是 20261103 601328 的形状（+3.75% 浮盈翻成 -2.28% 实现）。
//
// 安全垫的口径要说清：定参给的公式把 1pp 加在**价格侧**（arm），落到净收益是
// pad×(1−d)=0.95%，不是 1.00%。这里按公式实现并在测试里钉死这个换算，
// 免得日后有人"顺手修正"成收益侧、把武装线抬高到 8.05% 而以为只是四舍五入。
func TestTrailingArmPctForCoversFriction(t *testing.T) {
	p := DefaultParams(model.FromFloat(12000))
	d := p.TrailingStopPct
	for _, yuan := range []float64{595, 639, 821, 1010, 2000, 5000, 10000} {
		notional := model.FromFloat(yuan)
		f := FrictionPct(notional)
		arm := p.TrailingArmPctFor(notional)
		net := (1+arm)*(1-d) - 1 - f
		if net <= 0 {
			t.Errorf("%.0f 元：arm=%.4f 触发后净毛利 %.4f%%，仍为非正（旧缺陷复发）", yuan, arm, net*100)
		}
		if arm < p.TrailingArmPct-1e-9 {
			t.Errorf("%.0f 元：arm=%.4f 低于静态下限 %.4f", yuan, arm, p.TrailingArmPct)
		}
		// 动态项绑定时（小额单）净毛利恰好是一个价格侧安全垫换算到收益侧。
		if arm > p.TrailingArmPct {
			if want := p.TrailingArmSafetyPct * (1 - d); net < want-1e-9 {
				t.Errorf("%.0f 元：arm=%.4f 动态绑定但净毛利 %.4f%% < 安全垫换算 %.4f%%",
					yuan, arm, net*100, want*100)
			}
		}
	}
	// 小额单的武装线必须严格高于静态下限（595 元那笔正是它把闸抬到 8.14%）。
	small := p.TrailingArmPctFor(model.FromFloat(595))
	if small <= p.TrailingArmPct {
		t.Errorf("595 元单子 arm=%.4f，应高于静态下限 %.4f", small, p.TrailingArmPct)
	}
	// 旧参数为什么注定亏：arm=6% 时这两笔触发后都是净亏。
	for _, yuan := range []float64{595, 639} {
		if net := (1+0.06)*(1-d) - 1 - FrictionPct(model.FromFloat(yuan)); net >= 0 {
			t.Errorf("旧参数复核失败：arm=6%% 时 %.0f 元净毛利=%.4f%%，应为负", yuan, net*100)
		}
	}
	// arm=0 是显式回退开关，必须退回旧口径而不是被动态项覆盖。
	legacy := p
	legacy.TrailingArmPct = 0
	if got := legacy.TrailingArmPctFor(model.FromFloat(595)); got != 0 {
		t.Errorf("TrailingArmPct=0 应返回 0（旧口径），实际 %.4f", got)
	}
	// 回撤线坏值（0 或 ≥1）不能把分母变成 0/负数：回落基准 5% 后仍是正毛利。
	bad := p
	bad.TrailingStopPct = 0
	if got := bad.TrailingArmPctFor(model.FromFloat(595)); got < p.TrailingArmPct {
		t.Errorf("回撤线坏值应回落基准 5%%，实际 arm=%.4f", got)
	}
}
