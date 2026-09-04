package goal

import (
	"fmt"

	"jingzhe-trader/internal/model"
)

// TriggerRule 档位变更触发规则（作为变更日志的 trigger 字段）。
type TriggerRule string

const (
	TriggerNone             TriggerRule = "none"
	TriggerQuarterReset     TriggerRule = "quarter_reset"      // 季度首日强制重置 G1 + 清锁利
	TriggerManualOverride   TriggerRule = "manual_override"    // 人工 set_gear 覆盖
	TriggerBudgetTighten    TriggerRule = "budget_tighten"     // 回撤预算 ≥ 0.70：G1→G2（立即）
	TriggerBudgetDefend     TriggerRule = "budget_defend"      // 回撤预算 ≥ 1.00：*→G3（立即）
	TriggerRecoverG2        TriggerRule = "upgrade_recover_g2" // G3→G2：连续 3 日 < 0.85
	TriggerRecoverG1        TriggerRule = "upgrade_recover_g1" // G2→G1：连续 3 日 < 0.55
	TriggerProfitLockOn     TriggerRule = "profit_lock_on"     // 锁利触发
	TriggerAlreadyEvaluated TriggerRule = "already_evaluated"  // 当日已评估（每日至多一次）
)

// State 档位状态机输入状态（与 goal.state 的字段对应，剥离持久化字段）。
type State struct {
	Gear          model.Gear
	ProfitLock    bool
	UpgradeStreak int
}

// EvalOptions 评估上下文选项。
type EvalOptions struct {
	Today         string // 本次评估交易日（YYYYMMDD）
	LastEvalDate  string // 上次评估交易日（空 = 首次）
	OverrideGear  string // 人工覆盖档位（空 = 无覆盖）
	OverrideUntil string // 覆盖有效期（YYYYMMDD，含当日）
	QuarterReset  bool   // 本次评估日为季度首日（服务层按季度标签变化判定）
}

// GearConfig 档位状态机阈值（来自 config goal.* 键，非档位参数本身）。
type GearConfig struct {
	TightenAtBudget   float64 // G1→G2 触发预算（默认 0.70）
	DefendAtBudget    float64 // G3 触发预算（默认 1.00）
	UpgradeHysteresis float64 // 升档迟滞带（默认 0.15：G3→G2 需 <0.85、G2→G1 需 <0.55）
	UpgradeDays       int     // 连续满足天数（默认 3）
	LockAtProgress    float64 // 锁利触发进度（默认 1.00）
	LockBudgetBelow   float64 // 锁利要求预算耗用上限（默认 0.70）
}

// DefaultGearConfig 默认阈值（与 config/keys.go 默认值一致）。
func DefaultGearConfig() GearConfig {
	return GearConfig{
		TightenAtBudget:   0.70,
		DefendAtBudget:    1.00,
		UpgradeHysteresis: 0.15,
		UpgradeDays:       3,
		LockAtProgress:    1.00,
		LockBudgetBelow:   0.70,
	}
}

// Decision 状态机决策输出。
type Decision struct {
	From      model.Gear
	To        model.Gear
	FromLock  bool
	ToLock    bool
	NewStreak int
	Trigger   TriggerRule
	IsManual  bool
	Changed   bool // 是否产生状态转移/锁利变化（决定是否留痕）
	Reason    string
}

// Evaluate 档位状态机（纯函数，无 IO，§5.5 / 验收 §10.5-2）。
//
// 判定顺序（保证"每日至多一次转移"）：
//  1. 季度首日 → 强制重置 G1 + 清锁利 + streak 清零
//  2. 人工覆盖有效期内 → 使用覆盖档位并清除锁利（人工 set_gear 解除锁利）
//  3. 当日已评估 → 不做任何转移
//  4. 降档（立即）：预算 ≥ defend → G3；预算 ≥ tighten 且当前 G1 → G2
//  5. 升档（迟滞 + 连续 N 日）：G3 需 < defend−hysteresis；G2 需 < tighten−hysteresis
//  6. 锁利：Progress ≥ lockAtProgress 且预算 < lockBudgetBelow 且未锁 → 上锁（不与档位转移同日竞争触发名）
func Evaluate(st State, m GoalMetrics, cfg GearConfig, opt EvalOptions) Decision {
	dec := Decision{
		From:      st.Gear,
		To:        st.Gear,
		FromLock:  st.ProfitLock,
		ToLock:    st.ProfitLock,
		NewStreak: st.UpgradeStreak,
		Trigger:   TriggerNone,
	}

	// 1. 季度首日：强制重置（无论当前状态如何）
	if opt.QuarterReset {
		dec.To = model.GearG1
		dec.ToLock = false
		dec.NewStreak = 0
		dec.Trigger = TriggerQuarterReset
		dec.Changed = true
		dec.Reason = "季度首日：目标重置，档位回归 G1，清除锁利"
		return dec
	}

	// 2. 人工覆盖有效期内（OverrideUntil 含当日）
	if g, err := model.ParseGear(opt.OverrideGear); err == nil && opt.OverrideUntil >= opt.Today {
		dec.To = g
		dec.ToLock = false // 人工覆盖解除锁利（§8.2 锁利解除条件之二）
		dec.IsManual = true
		dec.Trigger = TriggerManualOverride
		dec.Changed = st.Gear != g || st.ProfitLock
		dec.Reason = fmt.Sprintf("人工覆盖生效至 %s", opt.OverrideUntil)
		return dec
	}

	// 3. 每日至多一次转移：当日已评估过则直接返回原状态
	if opt.LastEvalDate == opt.Today && opt.Today != "" {
		dec.Trigger = TriggerAlreadyEvaluated
		dec.Reason = "当日已评估，至多一次转移"
		return dec
	}

	bc := m.BudgetConsumed

	// 4a. 防守线：预算耗尽 → G3（立即，任何档位）
	if bc >= cfg.DefendAtBudget && st.Gear != model.GearG3 {
		dec.To = model.GearG3
		dec.NewStreak = 0
		dec.Trigger = TriggerBudgetDefend
		dec.Changed = true
		dec.Reason = fmt.Sprintf("回撤预算耗用 %.2f ≥ %.2f，立即转防守", bc, cfg.DefendAtBudget)
		return dec
	}

	// 4b. 收紧线：预算 ≥ tighten 且当前 G1 → G2（立即）
	if bc >= cfg.TightenAtBudget && st.Gear == model.GearG1 {
		dec.To = model.GearG2
		dec.NewStreak = 0
		dec.Trigger = TriggerBudgetTighten
		dec.Changed = true
		dec.Reason = fmt.Sprintf("回撤预算耗用 %.2f ≥ %.2f，立即收紧", bc, cfg.TightenAtBudget)
		return dec
	}

	// 5. 升档（连续 N 日 + 迟滞带，任一不满足 → streak 清零）
	recoverG2 := cfg.DefendAtBudget - cfg.UpgradeHysteresis  // G3→G2 需 < 0.85
	recoverG1 := cfg.TightenAtBudget - cfg.UpgradeHysteresis // G2→G1 需 < 0.55
	switch st.Gear {
	case model.GearG3:
		if bc < recoverG2 {
			dec.NewStreak = st.UpgradeStreak + 1
			if dec.NewStreak >= cfg.UpgradeDays {
				dec.To = model.GearG2
				dec.NewStreak = 0
				dec.Trigger = TriggerRecoverG2
				dec.Changed = true
				dec.Reason = fmt.Sprintf("预算耗用连续 %d 日 < %.2f，防守转收紧", cfg.UpgradeDays, recoverG2)
			}
			return dec
		}
		dec.NewStreak = 0
		return dec
	case model.GearG2:
		if bc < recoverG1 {
			dec.NewStreak = st.UpgradeStreak + 1
			if dec.NewStreak >= cfg.UpgradeDays {
				dec.To = model.GearG1
				dec.NewStreak = 0
				dec.Trigger = TriggerRecoverG1
				dec.Changed = true
				dec.Reason = fmt.Sprintf("预算耗用连续 %d 日 < %.2f，收紧转标准", cfg.UpgradeDays, recoverG1)
			}
			return dec
		}
		dec.NewStreak = 0
		return dec
	default: // G1 为最高档，无升档路径
		dec.NewStreak = 0
	}

	// 6. 锁利：进度达标且回撤预算充裕 → 上锁（未上锁才触发；仅季度重置/人工覆盖解除）
	if !st.ProfitLock && m.Progress >= cfg.LockAtProgress && bc < cfg.LockBudgetBelow {
		dec.ToLock = true
		dec.Trigger = TriggerProfitLockOn
		dec.Changed = true
		dec.Reason = fmt.Sprintf("进度 %.2f ≥ %.2f 且预算耗用 %.2f < %.2f，触发锁利",
			m.Progress, cfg.LockAtProgress, bc, cfg.LockBudgetBelow)
	}
	return dec
}
