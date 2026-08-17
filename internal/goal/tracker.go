// Package goal 季度目标跟踪: 进度监控 + 回撤预算 + 风险敞口自动调节
//
// 核心思想: 目标是约束而非预测。
//   - 每个日历季度设定收益目标 (如 +15%) 与最大回撤预算 (如 10%)
//   - 每日根据实盘账户快照计算: 目标进度、剩余时间所需收益率、回撤预算消耗
//   - 预算消耗超阈值 → 自动收紧风险敞口 (只收紧不放松)
//   - 目标提前达成 → 锁定利润模式, 降低敞口保住胜利果实
package goal

import (
	"fmt"
	"math"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

// 风险模式
const (
	ModeNormal    = "normal"    // 正常运行
	ModeTightened = "tightened" // 回撤预算消耗 >=70%, 收紧敞口
	ModeDefensive = "defensive" // 回撤预算耗尽, 防守模式 (近乎停止开新仓)
	ModeLocked    = "locked"    // 季度目标已达成, 锁定利润
)

// ModeLabel 风险模式中文名
func ModeLabel(mode string) string {
	switch mode {
	case ModeTightened:
		return "收紧"
	case ModeDefensive:
		return "防守"
	case ModeLocked:
		return "锁利"
	default:
		return "正常"
	}
}

// Status 季度目标状态快照
type Status struct {
	Date             string   `json:"date"`
	Quarter          string   `json:"quarter"`
	QuarterStart     string   `json:"quarter_start"`
	QuarterEnd       string   `json:"quarter_end"`
	BaselineAsset    float64  `json:"baseline_asset"` // 季初资产基准
	CurrentAsset     float64  `json:"current_asset"`  // 当前总资产
	ReturnPct        float64  `json:"return_pct"`     // 季度内收益率
	TargetPct        float64  `json:"target_pct"`     // 季度目标收益率
	Progress         float64  `json:"progress"`       // 目标完成度 (可>1)
	DaysElapsed      int      `json:"days_elapsed"`
	DaysTotal        int      `json:"days_total"`
	RequiredDailyPct float64  `json:"required_daily_pct"` // 达成目标所需剩余日均收益(估算)
	PeakAsset        float64  `json:"peak_asset"`         // 季度内资产峰值
	DrawdownPct      float64  `json:"drawdown_pct"`       // 当前回撤
	BudgetPct        float64  `json:"budget_pct"`         // 回撤预算
	BudgetConsumed   float64  `json:"budget_consumed"`    // 预算消耗比例
	Mode             string   `json:"mode"`
	ModeLabel        string   `json:"mode_label"`
	Notes            []string `json:"notes"`
}

// Tracker 季度目标跟踪器
type Tracker struct {
	cfg       config.GoalConfig
	tradeRepo *store.TradeRepo
	portRepo  *store.PortfolioRepo
	liveRunID string // 实盘快照 run_id
}

// NewTracker 创建目标跟踪器
func NewTracker(cfg config.GoalConfig, tradeRepo *store.TradeRepo, portRepo *store.PortfolioRepo, liveRunID string) *Tracker {
	return &Tracker{cfg: cfg, tradeRepo: tradeRepo, portRepo: portRepo, liveRunID: liveRunID}
}

// Enabled 是否启用
func (t *Tracker) Enabled() bool { return t.cfg.Enabled }

// QuarterOf 返回日期所在日历季度的标识/起止 (YYYYMMDD)
func QuarterOf(date string) (label, start, end string) {
	tm, err := time.Parse("20060102", date)
	if err != nil {
		return "", "", ""
	}
	q := (int(tm.Month())-1)/3 + 1
	startTM := time.Date(tm.Year(), time.Month((q-1)*3+1), 1, 0, 0, 0, 0, time.Local)
	endTM := startTM.AddDate(0, 3, -1)
	return fmt.Sprintf("%dQ%d", tm.Year(), q),
		startTM.Format("20060102"), endTM.Format("20060102")
}

// Status 计算指定日期的季度目标状态
func (t *Tracker) Status(date string, currentAsset float64) (*Status, error) {
	label, qStart, qEnd := QuarterOf(date)
	if label == "" {
		return nil, fmt.Errorf("日期格式错误: %s", date)
	}
	st := &Status{
		Date:         date,
		Quarter:      label,
		QuarterStart: qStart,
		QuarterEnd:   qEnd,
		TargetPct:    t.cfg.QuarterlyTargetPct,
		BudgetPct:    t.cfg.MaxDrawdownBudget,
		CurrentAsset: currentAsset,
		Mode:         ModeNormal,
	}

	// 季度时间进度
	if tm, err := time.Parse("20060102", date); err == nil {
		startTM, _ := time.Parse("20060102", qStart)
		endTM, _ := time.Parse("20060102", qEnd)
		st.DaysTotal = int(endTM.Sub(startTM).Hours()/24) + 1
		st.DaysElapsed = int(tm.Sub(startTM).Hours()/24) + 1
		if st.DaysElapsed > st.DaysTotal {
			st.DaysElapsed = st.DaysTotal
		}
	}

	// 季初基准: 季度开始前最后一个实盘快照; 无则退回初始资金
	snaps, err := t.tradeRepo.GetAccountSnapshotsByRunID(t.liveRunID)
	if err != nil {
		return nil, fmt.Errorf("查询实盘快照失败: %w", err)
	}
	baseline := 0.0
	peak := 0.0
	for _, s := range snaps {
		if s.TradeDate < qStart {
			baseline = s.TotalAsset // 快照按日期升序, 季度前最后一个即季初基准
		} else if s.TradeDate <= date && s.TotalAsset > peak {
			peak = s.TotalAsset
		}
	}
	if baseline <= 0 {
		if v, _ := t.portRepo.GetMeta("initial_capital"); v != "" {
			fmt.Sscanf(v, "%f", &baseline)
		}
	}
	st.BaselineAsset = baseline
	// 峰值从季初基准起算: 季度首日资产就是当时的峰值, 否则季初暴跌会漏计回撤
	if baseline > peak {
		peak = baseline
	}
	if currentAsset > peak {
		peak = currentAsset
	}
	st.PeakAsset = peak

	// 收益与进度
	if baseline > 0 {
		st.ReturnPct = (currentAsset - baseline) / baseline
	}
	if st.TargetPct > 0 {
		st.Progress = st.ReturnPct / st.TargetPct
	}

	// 达成目标所需剩余日均收益 (剩余自然日按 5/7 折算交易日, 粗略但直观)
	if st.TargetPct > 0 && baseline > 0 && currentAsset > 0 && st.DaysElapsed < st.DaysTotal {
		remainDays := st.DaysTotal - st.DaysElapsed
		needTotal := (baseline * (1 + st.TargetPct)) / currentAsset
		if needTotal > 0 {
			tradeDays := float64(remainDays) * 5.0 / 7.0
			if tradeDays >= 1 {
				st.RequiredDailyPct = math.Pow(needTotal, 1.0/tradeDays) - 1
			}
		}
	}

	// 回撤与预算消耗
	if peak > 0 {
		st.DrawdownPct = (peak - currentAsset) / peak
	}
	if st.BudgetPct > 0 {
		st.BudgetConsumed = st.DrawdownPct / st.BudgetPct
	}

	// 风险模式判定
	switch {
	case st.BudgetPct > 0 && st.BudgetConsumed >= 1.0:
		st.Mode = ModeDefensive
		st.Notes = append(st.Notes, fmt.Sprintf("回撤 %.1f%% 已耗尽 %.1f%% 预算, 进入防守模式", st.DrawdownPct*100, st.BudgetPct*100))
	case st.BudgetPct > 0 && st.BudgetConsumed >= 0.7:
		st.Mode = ModeTightened
		st.Notes = append(st.Notes, fmt.Sprintf("回撤预算消耗 %.0f%%, 收紧风险敞口", st.BudgetConsumed*100))
	case st.TargetPct > 0 && st.Progress >= 1.0:
		st.Mode = ModeLocked
		st.Notes = append(st.Notes, fmt.Sprintf("季度目标已达成 (%.1f%%/%.1f%%), 锁定利润", st.ReturnPct*100, st.TargetPct*100))
	}
	st.ModeLabel = ModeLabel(st.Mode)
	return st, nil
}

// AdjustRisk 按目标状态调节风控配置 (只收紧, 不放松)
// 返回调整后的配置与调整说明 (无调整时说明为空)
func (t *Tracker) AdjustRisk(base config.RiskConfig, st *Status) (config.RiskConfig, []string) {
	if !t.cfg.Enabled || !t.cfg.AutoAdjust || st == nil {
		return base, nil
	}
	adj := base
	var notes []string
	switch st.Mode {
	case ModeDefensive:
		if adj.MaxTotalPositionPct > 0.2 {
			adj.MaxTotalPositionPct = 0.2
			notes = append(notes, "回撤预算耗尽: 总仓位上限压至20%")
		}
		if adj.StopLossPct > 0.05 {
			adj.StopLossPct = 0.05
			notes = append(notes, "回撤预算耗尽: 止损收紧至5%")
		}
	case ModeTightened:
		tightened := base.MaxTotalPositionPct * 0.6
		if adj.MaxTotalPositionPct > tightened {
			adj.MaxTotalPositionPct = tightened
			notes = append(notes, fmt.Sprintf("回撤预算消耗超70%%: 总仓位上限收紧至 %.0f%%", tightened*100))
		}
	case ModeLocked:
		locked := base.MaxTotalPositionPct * 0.5
		if adj.MaxTotalPositionPct > locked {
			adj.MaxTotalPositionPct = locked
			notes = append(notes, fmt.Sprintf("季度目标已达成: 总仓位上限降至 %.0f%% 锁定利润", locked*100))
		}
	}
	return adj, notes
}
