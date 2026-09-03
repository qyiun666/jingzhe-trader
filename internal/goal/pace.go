package goal

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/risk"
)

// PaceSettings 落后策略配置（来自 config goal.pace_* 键）。
type PaceSettings struct {
	Policy      string  // unrestricted（默认）/ conservative / aggressive
	MaxBoostPct float64 // aggressive 放大上限（三重保护①，默认 0.10）
	BudgetBelow float64 // aggressive 仅在预算耗用低于该值时允许（三重保护②，默认 0.30）
}

// AggressivePace 激进落后策略（goal.pace_policy = aggressive，§8.3 三重保护）：
//  ① 放大幅度 +10% 封顶（MaxBoostPct 超过 0.10 一律截断到 0.10）；
//  ② 仅在 BudgetConsumed < BudgetBelow 时允许加成；
//  ③ 每日人工确认续期（Confirmed = pace_confirm_date == today），过期自动回落档位原值。
// 未落后（PaceGap < 0.15）不加码；被拒时回落档位原值（等于无 pace 加成）。
type AggressivePace struct {
	PaceGap        float64
	BudgetConsumed float64
	MaxBoostPct    float64
	BudgetBelow    float64
	Confirmed      bool
}

// maxBoostCap 三重保护①的硬封顶：即使配置更大，放大也 ≤ +10%。
const maxBoostCap = 0.10

// Apply 三重保护校验通过时按封顶比例放大总仓位上限，否则原样返回（回落）。
func (a AggressivePace) Apply(p risk.RiskParams) risk.RiskParams {
	if a.PaceGap < 0.15 {
		return p // 不落后不加码
	}
	// 三重保护②③：预算不足或当日未人工确认 → 拒绝加成，回落档位原值
	if a.BudgetConsumed >= a.BudgetBelow {
		return p
	}
	if !a.Confirmed {
		return p
	}
	boost := a.MaxBoostPct
	if boost > maxBoostCap {
		boost = maxBoostCap // 三重保护①：放大 > +10% 一律截断
	}
	if boost <= 0 {
		return p
	}
	// min(gear×(1+boost), gear+boost) 双重封顶；最终仍受 Resolve 物理熔断收口约束
	capped := p.MaxTotalPositionPct * (1 + boost)
	if lim := p.MaxTotalPositionPct + boost; capped > lim {
		capped = lim
	}
	p.MaxTotalPositionPct = capped
	return p
}

// Name 策略名。
func (AggressivePace) Name() string { return "aggressive" }

// AggressiveDenial 激进模式被拒时的显式原因（供告警 PACE_BOOST_EXPIRED / PACE_BOOST_DENIED）。
func AggressiveDenied(s PaceSettings, m GoalMetrics, paceConfirmDate, today string) (code, reason string, ok bool) {
	if s.Policy != PolicyAggressive {
		return "", "", false
	}
	if m.PaceGap < 0.15 {
		return "", "", false // 未落后，无需加成，不算拒绝
	}
	if m.BudgetConsumed >= s.BudgetBelow {
		return "PACE_BOOST_DENIED", fmt.Sprintf("回撤预算耗用 %.2f ≥ %.2f，激进加成被拒（三重保护②）", m.BudgetConsumed, s.BudgetBelow), true
	}
	if today == "" || paceConfirmDate != today {
		return "PACE_BOOST_EXPIRED", fmt.Sprintf("pace_confirm_date=%s != 今日 %s，激进加成过期回落（三重保护③）", paceConfirmDate, today), true
	}
	return "", "", false
}

// 策略名常量（与 config goal.pace_policy 取值一致）。
const (
	PolicyUnrestricted  = "unrestricted"
	PolicyConservative  = "conservative"
	PolicyAggressive    = "aggressive"
)

// NewPaceAdjust 装配落后策略为 risk.PaceAdjust（Resolve 链路的最后一个参数变换器）。
// paceConfirmDate 为 goal_state.pace_confirm_date；today 为本次评估交易日。
func NewPaceAdjust(s PaceSettings, m GoalMetrics, paceConfirmDate, today string) risk.PaceAdjust {
	switch strings.TrimSpace(s.Policy) {
	case PolicyConservative:
		return risk.ConservativePace{PaceGap: m.PaceGap}
	case PolicyAggressive:
		return AggressivePace{
			PaceGap:        m.PaceGap,
			BudgetConsumed: m.BudgetConsumed,
			MaxBoostPct:    s.MaxBoostPct,
			BudgetBelow:    s.BudgetBelow,
			Confirmed:      today != "" && paceConfirmDate == today,
		}
	default: // unrestricted（用户拍板默认）
		return risk.UnrestrictedPace{PaceGap: m.PaceGap, BudgetConsumed: m.BudgetConsumed}
	}
}
