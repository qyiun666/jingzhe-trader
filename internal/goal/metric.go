// Package goal 季度目标域：三度量计算、档位状态机（纯函数）、落后策略装配与持久化编排。
//
// 核心原则（ARCHITECTURE §8 / 验收 §10.5-2）：
//   - Evaluate 是纯函数：升档/降档/迟滞/季度重置/人工覆盖全部写在同一函数内，
//     "忘了写恢复路径"在结构上不可能发生（根治历史 D2 单向棘轮）；
//   - goal 包只做计算与状态驱动，不重复定义档位参数表（GearTable 唯一定义处是 risk/params.go）；
//   - 每日至多一次状态转移。
//
// 依赖方向：goal 依赖 model / risk / store / market，无网络 IO。
package goal

import (
	"jingzhe-trader/internal/model"
)

// GoalMetrics 三度量 + 进度度量（§8.1）。全部为派生值，不落库。
type GoalMetrics struct {
	Quarter        string    // 季度标签，如 2026Q3
	CurrentAsset   model.Fen // 当前总资产（分）
	BaselineAsset  model.Fen // 季初基准（分）
	PeakAsset      model.Fen // 季内峰值（分，从季初基准起算）
	TargetPct      float64   // 季度目标收益率（如 0.15）
	BudgetPct      float64   // 回撤预算（如 0.10）
	ReturnPct      float64   // (cur - baseline) / baseline
	Progress       float64   // ReturnPct / TargetPct
	DrawdownPct    float64   // (peak - cur) / peak
	BudgetConsumed float64   // DrawdownPct / BudgetPct
	TimeProgress   float64   // 已过交易日 / 季度总交易日
	PaceGap        float64   // TimeProgress - Progress（>0 表示落后）
	StaleDays      int       // 快照陈旧天数（0 = 当日快照）
	ElapsedDays    int       // 季度内已过交易日数
	TotalDays      int       // 季度总交易日数
}

// ComputeMetrics 三度量计算（纯函数，无 IO）。
// baseline <= 0 时全部比例置 0（新季度尚未建立基准，不能给出失真读数）。
func ComputeMetrics(baseline, peak, current model.Fen, targetPct, budgetPct float64, elapsed, total int) GoalMetrics {
	m := GoalMetrics{
		CurrentAsset:  current,
		BaselineAsset: baseline,
		PeakAsset:     peak,
		TargetPct:     targetPct,
		BudgetPct:     budgetPct,
		ElapsedDays:   elapsed,
		TotalDays:     total,
	}
	if baseline <= 0 || targetPct <= 0 {
		return m // 无有效基准：度量全零，由调用方显式降级
	}
	cur := float64(current)
	base := float64(baseline)
	peakF := float64(peak)
	if peakF < base {
		peakF = base // 峰值从季初基准起算
	}
	m.ReturnPct = (cur - base) / base
	m.Progress = m.ReturnPct / targetPct
	if peakF > 0 {
		m.DrawdownPct = (peakF - cur) / peakF
	}
	if m.DrawdownPct < 0 {
		m.DrawdownPct = 0 // 创新高时回撤为 0，不允许负数污染预算读数
	}
	if budgetPct > 0 {
		m.BudgetConsumed = m.DrawdownPct / budgetPct
	}
	if total > 0 {
		m.TimeProgress = float64(elapsed) / float64(total)
	}
	m.PaceGap = m.TimeProgress - m.Progress
	return m
}
