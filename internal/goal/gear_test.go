package goal

import (
	"math"
	"testing"

	"jingzhe-trader/internal/model"
)

// ---------- 档位状态机 Evaluate：表驱动验收（§10.5-2，≥30 例覆盖全部分支）----------

type gearCase struct {
	name        string
	st          State
	bc          float64 // GoalMetrics.BudgetConsumed
	prog        float64 // GoalMetrics.Progress
	cfg         GearConfig
	opt         EvalOptions
	wantTo      model.Gear
	wantLock    bool
	wantStreak  int
	wantTrig    TriggerRule
	wantManual  bool
	wantChanged bool
}

func defaultCfg() GearConfig { return DefaultGearConfig() }

func buildCases() []gearCase {
	c := defaultCfg()
	return []gearCase{
		// 1) 季度首日：G2 -> G1，清锁利，streak 清零
		{name: "qr_g2_to_g1", st: State{Gear: model.GearG2, ProfitLock: true, UpgradeStreak: 5},
			cfg: c, opt: EvalOptions{Today: "20260701", QuarterReset: true},
			wantTo: model.GearG1, wantLock: false, wantStreak: 0, wantTrig: TriggerQuarterReset, wantChanged: true},
		// 2) 季度首日：G3 + 已锁利 -> G1，锁利解除
		{name: "qr_g3_lock_cleared", st: State{Gear: model.GearG3, ProfitLock: true, UpgradeStreak: 2},
			cfg: c, opt: EvalOptions{Today: "20260701", QuarterReset: true},
			wantTo: model.GearG1, wantLock: false, wantStreak: 0, wantTrig: TriggerQuarterReset, wantChanged: true},
		// 3) 人工覆盖（有效期内）G3，原 G1 已锁利 -> 落到 G3，解除锁利
		{name: "ov_g3_over_locked_g1", st: State{Gear: model.GearG1, ProfitLock: true, UpgradeStreak: 0},
			cfg: c, opt: EvalOptions{Today: "20260702", OverrideGear: "G3", OverrideUntil: "20260702"},
			wantTo: model.GearG3, wantLock: false, wantStreak: 0, wantTrig: TriggerManualOverride, wantManual: true, wantChanged: true},
		// 4) 人工覆盖 G2，原 G2 无锁利 -> 档位相同且无锁利变化 -> 未变更
		{name: "ov_g2_same_no_change", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 0},
			cfg: c, opt: EvalOptions{Today: "20260702", OverrideGear: "G2", OverrideUntil: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerManualOverride, wantManual: true, wantChanged: false},
		// 5) 人工覆盖 G2，原 G2 已锁利 -> 解除锁利 -> 变更
		{name: "ov_g2_unlock", st: State{Gear: model.GearG2, ProfitLock: true, UpgradeStreak: 0},
			cfg: c, opt: EvalOptions{Today: "20260702", OverrideGear: "G2", OverrideUntil: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerManualOverride, wantManual: true, wantChanged: true},
		// 6) 人工覆盖已过期（until < today）-> 不生效，走正常逻辑（G1 小回撤无变更）
		{name: "ov_expired_falls_through", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.10, prog: 0.50, cfg: c, opt: EvalOptions{Today: "20260702", OverrideGear: "G3", OverrideUntil: "20260701"},
			wantTo: model.GearG1, wantLock: false, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 7) 当日已评估 -> 即便 bc 爆表也不转移（每日至多一次）
		{name: "already_evaluated_blocks_defend", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 0},
			bc: 1.00, cfg: c, opt: EvalOptions{Today: "20260702", LastEvalDate: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerAlreadyEvaluated, wantChanged: false},
		// 8) 防守线：G1 bc=1.00 -> G3
		{name: "defend_g1", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 1.00, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG3, wantLock: false, wantStreak: 0, wantTrig: TriggerBudgetDefend, wantChanged: true},
		// 9) 防守线：G2 bc=1.00 -> G3
		{name: "defend_g2", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 0},
			bc: 1.00, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG3, wantLock: false, wantStreak: 0, wantTrig: TriggerBudgetDefend, wantChanged: true},
		// 10) 已是 G3 且 bc=1.00 -> 无变更（无冗余转移）
		{name: "defend_already_g3", st: State{Gear: model.GearG3, ProfitLock: false, UpgradeStreak: 0},
			bc: 1.00, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG3, wantLock: false, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 11) 收紧线：G1 bc=0.70 -> G2
		{name: "tighten_g1_070", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.70, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerBudgetTighten, wantChanged: true},
		// 12) 收紧线：G1 bc=0.85 -> G2
		{name: "tighten_g1_085", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.85, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerBudgetTighten, wantChanged: true},
		// 13) G2 bc=0.70 -> 不触发收紧（仅 G1 收紧），且不满足回升 -> 无变更
		{name: "tighten_not_for_g2", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.70, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 14) G3 bc=0.84 streak=2 -> 第 3 日 -> G2
		{name: "recover_g3_s2", st: State{Gear: model.GearG3, ProfitLock: false, UpgradeStreak: 2},
			bc: 0.84, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerRecoverG2, wantChanged: true},
		// 15) G3 bc=0.84 streak=0 -> streak=1，未到阈值
		{name: "recover_g3_s0", st: State{Gear: model.GearG3, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.84, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG3, wantLock: false, wantStreak: 1, wantTrig: TriggerNone, wantChanged: false},
		// 16) G3 bc=0.84 streak=1 -> streak=2
		{name: "recover_g3_s1", st: State{Gear: model.GearG3, ProfitLock: false, UpgradeStreak: 1},
			bc: 0.84, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG3, wantLock: false, wantStreak: 2, wantTrig: TriggerNone, wantChanged: false},
		// 17) G3 bc=0.90（≥0.85）-> streak 清零，无变更
		{name: "recover_g3_reset", st: State{Gear: model.GearG3, ProfitLock: false, UpgradeStreak: 2},
			bc: 0.90, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG3, wantLock: false, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 18) G3 bc=0.84 streak=3 -> 已满足条件，立即 G2 并清零
		{name: "recover_g3_s3", st: State{Gear: model.GearG3, ProfitLock: false, UpgradeStreak: 3},
			bc: 0.84, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerRecoverG2, wantChanged: true},
		// 19) G2 bc=0.50 streak=2 -> 第 3 日 -> G1
		{name: "recover_g2_s2", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 2},
			bc: 0.50, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG1, wantLock: false, wantStreak: 0, wantTrig: TriggerRecoverG1, wantChanged: true},
		// 20) G2 bc=0.50 streak=0 -> streak=1
		{name: "recover_g2_s0", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.50, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 1, wantTrig: TriggerNone, wantChanged: false},
		// 21) G2 bc=0.60（≥0.55）-> streak 清零
		{name: "recover_g2_reset", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 2},
			bc: 0.60, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 22) G2 bc=0.54 streak=1 -> streak=2
		{name: "recover_g2_s1", st: State{Gear: model.GearG2, ProfitLock: false, UpgradeStreak: 1},
			bc: 0.54, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 2, wantTrig: TriggerNone, wantChanged: false},
		// 23) G1 小回撤、进度不足 -> 无变更
		{name: "g1_quiet", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.10, prog: 0.50, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG1, wantLock: false, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 24) G1 进度达标 + 回撤预算充裕 -> 触发锁利
		{name: "lock_on", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.10, prog: 1.00, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG1, wantLock: true, wantStreak: 0, wantTrig: TriggerProfitLockOn, wantChanged: true},
		// 25) G1 进度达标但回撤预算已耗（>=0.70）：收紧线先于锁利触发 -> G2（防御性收紧优先，锁利不生效）
		{name: "tighten_prevails_over_lock_when_bc_high", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.80, prog: 1.00, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerBudgetTighten, wantChanged: true},
		// 26) G1 进度不足 -> 不锁利
		{name: "lock_blocked_by_progress", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.10, prog: 0.90, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG1, wantLock: false, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 27) G1 已锁利且进度达标 -> 维持锁利，无新变更
		{name: "lock_already_on", st: State{Gear: model.GearG1, ProfitLock: true, UpgradeStreak: 0},
			bc: 0.10, prog: 1.00, cfg: c, opt: EvalOptions{Today: "20260702"},
			wantTo: model.GearG1, wantLock: true, wantStreak: 0, wantTrig: TriggerNone, wantChanged: false},
		// 28) 自定义收紧阈值 0.50：G1 bc=0.50 -> G2
		{name: "custom_tighten_050", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.50, cfg: GearConfig{TightenAtBudget: 0.50, DefendAtBudget: 1.00, UpgradeHysteresis: 0.15, UpgradeDays: 3, LockAtProgress: 1.00, LockBudgetBelow: 0.70},
			opt: EvalOptions{Today: "20260702"}, wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerBudgetTighten, wantChanged: true},
		// 29) 自定义回升天数=1：G3 bc=0.84 streak=0 -> 当日即 G2
		{name: "custom_upgradedays_1", st: State{Gear: model.GearG3, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.84, cfg: GearConfig{TightenAtBudget: 0.70, DefendAtBudget: 1.00, UpgradeHysteresis: 0.15, UpgradeDays: 1, LockAtProgress: 1.00, LockBudgetBelow: 0.70},
			opt: EvalOptions{Today: "20260702"}, wantTo: model.GearG2, wantLock: false, wantStreak: 0, wantTrig: TriggerRecoverG2, wantChanged: true},
		// 30) 自定义防守阈值 0.80：G1 bc=0.80 -> G3
		{name: "custom_defend_080", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.80, cfg: GearConfig{TightenAtBudget: 0.70, DefendAtBudget: 0.80, UpgradeHysteresis: 0.15, UpgradeDays: 3, LockAtProgress: 1.00, LockBudgetBelow: 0.70},
			opt: EvalOptions{Today: "20260702"}, wantTo: model.GearG3, wantLock: false, wantStreak: 0, wantTrig: TriggerBudgetDefend, wantChanged: true},
		// 31) 自定义锁利阈值 0.80：G1 prog=0.80 -> 锁利
		{name: "custom_lock_080", st: State{Gear: model.GearG1, ProfitLock: false, UpgradeStreak: 0},
			bc: 0.10, prog: 0.80, cfg: GearConfig{TightenAtBudget: 0.70, DefendAtBudget: 1.00, UpgradeHysteresis: 0.15, UpgradeDays: 3, LockAtProgress: 0.80, LockBudgetBelow: 0.70},
			opt: EvalOptions{Today: "20260702"}, wantTo: model.GearG1, wantLock: true, wantStreak: 0, wantTrig: TriggerProfitLockOn, wantChanged: true},
		// 32) 季度重置优先于人工覆盖
		{name: "qr_beats_override", st: State{Gear: model.GearG3, ProfitLock: true, UpgradeStreak: 4},
			cfg: c, opt: EvalOptions{Today: "20260701", QuarterReset: true, OverrideGear: "G2", OverrideUntil: "20260701"},
			wantTo: model.GearG1, wantLock: false, wantStreak: 0, wantTrig: TriggerQuarterReset, wantManual: false, wantChanged: true},
	}
}

func TestEvaluateStateMachine(t *testing.T) {
	cases := buildCases()
	if len(cases) < 30 {
		t.Fatalf("验收要求 ≥30 例，实际 %d", len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := GoalMetrics{BudgetConsumed: tc.bc, Progress: tc.prog}
			dec := Evaluate(tc.st, m, tc.cfg, tc.opt)
			if dec.To != tc.wantTo {
				t.Errorf("To = %s, want %s", dec.To, tc.wantTo)
			}
			if dec.ToLock != tc.wantLock {
				t.Errorf("ToLock = %v, want %v", dec.ToLock, tc.wantLock)
			}
			if dec.NewStreak != tc.wantStreak {
				t.Errorf("NewStreak = %d, want %d", dec.NewStreak, tc.wantStreak)
			}
			if dec.Trigger != tc.wantTrig {
				t.Errorf("Trigger = %s, want %s", dec.Trigger, tc.wantTrig)
			}
			if dec.IsManual != tc.wantManual {
				t.Errorf("IsManual = %v, want %v", dec.IsManual, tc.wantManual)
			}
			if dec.Changed != tc.wantChanged {
				t.Errorf("Changed = %v, want %v", dec.Changed, tc.wantChanged)
			}
		})
	}
}

// ---------- 三度量 ComputeMetrics 边界（§10.5-1）----------

func TestComputeMetrics(t *testing.T) {
	approx := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

	t.Run("baseline<=0 全零", func(t *testing.T) {
		m := ComputeMetrics(0, 0, 5000, 0.15, 0.10, 10, 60)
		if m.ReturnPct != 0 || m.Progress != 0 || m.DrawdownPct != 0 || m.BudgetConsumed != 0 {
			t.Errorf("应全零，实际 %+v", m)
		}
	})
	t.Run("target<=0 全零", func(t *testing.T) {
		m := ComputeMetrics(10000, 10000, 5000, 0, 0.10, 10, 60)
		if m.ReturnPct != 0 || m.Progress != 0 {
			t.Errorf("应全零，实际 %+v", m)
		}
	})
	t.Run("normal", func(t *testing.T) {
		m := ComputeMetrics(10000, 11000, 10500, 0.15, 0.10, 10, 60)
		if !approx(m.ReturnPct, 0.05) {
			t.Errorf("ReturnPct=%v", m.ReturnPct)
		}
		if !approx(m.Progress, 0.05/0.15) {
			t.Errorf("Progress=%v", m.Progress)
		}
		if !approx(m.DrawdownPct, 500.0/11000.0) {
			t.Errorf("DrawdownPct=%v", m.DrawdownPct)
		}
		if !approx(m.BudgetConsumed, (500.0/11000.0)/0.10) {
			t.Errorf("BudgetConsumed=%v", m.BudgetConsumed)
		}
		if !approx(m.TimeProgress, 10.0/60.0) {
			t.Errorf("TimeProgress=%v", m.TimeProgress)
		}
		if !approx(m.PaceGap, 10.0/60.0-0.05/0.15) {
			t.Errorf("PaceGap=%v", m.PaceGap)
		}
	})
	t.Run("peak<baseline 钳制", func(t *testing.T) {
		// baseline=10000, peak=9000, current=11000 -> peakF=10000, 创新高回撤=0
		m := ComputeMetrics(10000, 9000, 11000, 0.15, 0.10, 10, 60)
		if m.DrawdownPct != 0 {
			t.Errorf("创新高回撤应为0，实际 %v", m.DrawdownPct)
		}
		if !approx(m.ReturnPct, 0.10) {
			t.Errorf("ReturnPct=%v", m.ReturnPct)
		}
	})
	t.Run("回撤为负钳制为0", func(t *testing.T) {
		m := ComputeMetrics(10000, 10000, 12000, 0.15, 0.10, 10, 60)
		if m.DrawdownPct != 0 {
			t.Errorf("DrawdownPct 应为0，实际 %v", m.DrawdownPct)
		}
	})
	t.Run("budget<=0 不除零", func(t *testing.T) {
		m := ComputeMetrics(10000, 11000, 10500, 0.15, 0, 10, 60)
		if m.BudgetConsumed != 0 {
			t.Errorf("BudgetConsumed 应为0，实际 %v", m.BudgetConsumed)
		}
	})
	t.Run("total<=0 时间进度为0", func(t *testing.T) {
		m := ComputeMetrics(10000, 10000, 10500, 0.15, 0.10, 10, 0)
		if m.TimeProgress != 0 {
			t.Errorf("TimeProgress 应为0，实际 %v", m.TimeProgress)
		}
	})
}
