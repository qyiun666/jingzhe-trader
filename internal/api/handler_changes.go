package api

import (
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// ChangeReport 变更检测报告
type ChangeReport struct {
	Date              string                    `json:"date"`
	DecisionChanges   []DecisionChangeJSON      `json:"decision_changes"`
	PlanStatusChanges []PlanStatusChangeJSON    `json:"plan_status_changes"`
	TaskStatus        map[string]TaskStatusJSON `json:"task_status"`
	Summary           string                    `json:"summary"`
}

// DecisionChangeJSON 决策变更 (API 输出格式)
type DecisionChangeJSON struct {
	TsCode         string  `json:"ts_code"`
	Name           string  `json:"name"`
	PrevDecision   string  `json:"prev_decision"`
	CurrDecision   string  `json:"curr_decision"`
	PrevConfidence float64 `json:"prev_confidence"`
	CurrConfidence float64 `json:"curr_confidence"`
	Detail         string  `json:"detail"`
}

// PlanStatusChangeJSON 计划状态变更
type PlanStatusChangeJSON struct {
	ID        int64   `json:"id"`
	TsCode    string  `json:"ts_code"`
	Name      string  `json:"name"`
	Direction string  `json:"direction"`
	OldStatus string  `json:"old_status"`
	NewStatus string  `json:"new_status"`
	Qty       int     `json:"qty"`
	RefPrice  float64 `json:"ref_price"`
}

// TaskStatusJSON 任务状态
type TaskStatusJSON struct {
	Completed  bool   `json:"completed"`
	LastRun    string `json:"last_run"`
	LastStatus string `json:"last_status"`
}

// BuildAgentChanges builds the change report for the given date.
func (s *Service) BuildAgentChanges(date string) *ChangeReport {
	if date == "" {
		// 默认使用数据库最新行情日期
		lastDate, err := s.barRepo.GetMaxTradeDate()
		if err != nil || lastDate == "" {
			return &ChangeReport{Summary: "无法确定日期且数据库无行情数据"}
		}
		date = lastDate
	} else {
		date = strings.ReplaceAll(strings.TrimSpace(date), "-", "")
	}

	report := &ChangeReport{
		Date:              date,
		DecisionChanges:   []DecisionChangeJSON{},
		PlanStatusChanges: []PlanStatusChangeJSON{},
		TaskStatus:        map[string]TaskStatusJSON{},
	}

	// 1. 决策变更检测
	todayDebates, err := s.debateRepo.GetByDate(date)
	if err != nil {
		logger.L().Warnw("查询当日辩论结果失败", "err", err)
	}
	if s.debateOrchestrator != nil && s.debateOrchestrator.IsEnabled() && len(todayDebates) > 0 {
		changes := s.debateOrchestrator.DetectDecisionChanges(todayDebates)
		for _, c := range changes {
			report.DecisionChanges = append(report.DecisionChanges, DecisionChangeJSON{
				TsCode:         c.TsCode,
				Name:           c.Name,
				PrevDecision:   c.PrevDecision,
				CurrDecision:   c.CurrDecision,
				PrevConfidence: c.PrevConfidence,
				CurrConfidence: c.CurrConfidence,
				Detail:         c.Detail,
			})
		}
	}

	// 2. 计划状态变更 (获取当日所有计划，对比初始状态和当前状态)
	plans, err := s.planRepo.GetPlansByDate(date)
	if err != nil {
		logger.L().Warnw("查询当日交易计划失败", "err", err)
	}
	for _, p := range plans {
		if p.Status != store.PlanStatusPending {
			// 状态已从 pending 变为其他
			report.PlanStatusChanges = append(report.PlanStatusChanges, PlanStatusChangeJSON{
				ID:        p.ID,
				TsCode:    p.TsCode,
				Name:      p.Name,
				Direction: p.Direction,
				OldStatus: store.PlanStatusPending,
				NewStatus: p.Status,
				Qty:       p.Qty,
				RefPrice:  p.RefPrice,
			})
		}
	}

	// 3. 任务完成状态
	today := time.Now().Format("20060102")
	for _, name := range store.JobNames {
		ts := TaskStatusJSON{Completed: false}
		if done, err := s.jobRepo.HasSucceeded(name, today); err == nil {
			ts.Completed = done
		}
		if run, err := s.jobRepo.LastSuccess(name); err == nil && run != nil {
			ts.LastRun = run.FinishedAt
			ts.LastStatus = run.Status
		}
		report.TaskStatus[name] = ts
	}

	// 4. 汇总 (复用统一的计划状态汇总)
	summary := s.buildPlanStatusSummary(plans)
	report.Summary = fmt.Sprintf("日期:%s | 辩论变更:%d | 计划:待确认%d/已确认%d/已执行%d | 任务完成:%v",
		date, len(report.DecisionChanges), summary.Pending, summary.Confirmed, summary.Executed,
		report.TaskStatus["signal"].Completed)

	return report
}
