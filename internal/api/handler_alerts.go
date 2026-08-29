package api

import (
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/store"
)

// BuildAgentAlerts returns recent or unread agent alerts.
func (s *Service) BuildAgentAlerts(unreadOnly bool, date string) map[string]interface{} {
	var alerts []store.AgentAlert
	var err error

	if unreadOnly {
		alerts, err = s.alertRepo.GetUnread()
	} else if date != "" {
		alerts, err = s.alertRepo.GetByDate(strings.ReplaceAll(strings.TrimSpace(date), "-", ""))
	} else {
		alerts, err = s.alertRepo.GetRecent(50)
	}

	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}

	// 附带汇总信息
	unreadCount := 0
	for _, a := range alerts {
		if a.Status == store.AlertStatusUnread {
			unreadCount++
		}
	}

	return map[string]interface{}{
		"alerts":       alerts,
		"total":        len(alerts),
		"unread_count": unreadCount,
	}
}

// MarkAlertsRead marks agent alerts as read. If all is true or id is zero,
// all alerts are marked read; otherwise the single specified alert is marked.
func (s *Service) MarkAlertsRead(all bool, id int64) (map[string]interface{}, error) {
	if all || id == 0 {
		n, err := s.alertRepo.MarkAllRead()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"marked_read": n,
			"message":     fmt.Sprintf("已标记 %d 条通知为已读", n),
		}, nil
	}

	if id > 0 {
		if err := s.alertRepo.MarkAsRead(id); err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"marked_read": 1,
			"id":          id,
		}, nil
	}

	return nil, fmt.Errorf("需要指定 id 或 all=true")
}

// BuildAgentDashboard builds the agent dashboard summary.
func (s *Service) BuildAgentDashboard() map[string]interface{} {
	today := time.Now().Format("20060102")

	// 未读通知
	unreadAlerts, _ := s.alertRepo.GetUnread()

	// 今日通知
	todayAlerts, _ := s.alertRepo.GetByDate(today)

	// 待处理计划
	openPlans, _ := s.planRepo.GetOpenPlans()

	// 任务完成状态
	taskStatus := map[string]bool{}
	for _, name := range store.JobNames {
		done, _ := s.jobRepo.HasSucceeded(name, today)
		taskStatus[name] = done
	}

	// 数据新鲜度
	lastDate, _ := s.barRepo.GetMaxTradeDate()

	// 辩论结果
	todayDebates, _ := s.debateRepo.GetByDate(lastDate)

	// 决策变更
	var decisionChanges interface{}
	if s.debateOrchestrator != nil && s.debateOrchestrator.IsEnabled() && len(todayDebates) > 0 {
		decisionChanges = s.debateOrchestrator.DetectDecisionChanges(todayDebates)
	} else {
		decisionChanges = []interface{}{}
	}

	// 计划状态汇总
	summary := s.buildPlanStatusSummary(openPlans)

	return map[string]interface{}{
		"date":             today,
		"data_last_date":   lastDate,
		"unread_alerts":    unreadAlerts,
		"today_alerts":     todayAlerts,
		"open_plans":       openPlans,
		"today_debates":    todayDebates,
		"decision_changes": decisionChanges,
		"task_completed":   taskStatus,
		"plan_summary": map[string]int{
			"pending":   summary.Pending,
			"confirmed": summary.Confirmed,
			"executed":  summary.Executed,
			"total":     len(openPlans),
		},
	}
}
