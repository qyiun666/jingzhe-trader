package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"jingzhe-trader/internal/store"
)

// HandleAgentAlerts GET /api/agent/alerts?date=&unread_only=true
// POST /api/agent/alerts  - 标记已读 {"id": N} 或 {"all": true}
// Agent 获取飞书通知的持久化副本, 用于离线读取和状态追踪
func (s *Service) HandleAgentAlerts(w http.ResponseWriter, r *http.Request) {
	alertRepo := store.NewAlertRepo(s.db)

	if r.Method == http.MethodPost {
		s.handleAlertsMarkRead(w, r, alertRepo)
		return
	}

	// GET: 查询通知
	date := r.URL.Query().Get("date")
	unreadOnly := r.URL.Query().Get("unread_only") == "true"

	var alerts []store.AgentAlert
	var err error

	if unreadOnly {
		alerts, err = alertRepo.GetUnread()
	} else if date != "" {
		alerts, err = alertRepo.GetByDate(parseDateParam(date))
	} else {
		alerts, err = alertRepo.GetRecent(50)
	}

	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询通知失败: "+err.Error())
		return
	}

	// 附带汇总信息
	unreadCount := 0
	for _, a := range alerts {
		if a.Status == store.AlertStatusUnread {
			unreadCount++
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"alerts":       alerts,
		"total":        len(alerts),
		"unread_count": unreadCount,
	})
}

// handleAlertsMarkRead 标记通知为已读
func (s *Service) handleAlertsMarkRead(w http.ResponseWriter, r *http.Request, alertRepo *store.AlertRepo) {
	var req struct {
		ID     int64 `json:"id"`      // 指定ID标记已读, 0=全部标记已读
		All    bool  `json:"all"`     // true=全部标记已读
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	if req.All || req.ID == 0 {
		n, err := alertRepo.MarkAllRead()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"marked_read": n,
			"message":     fmt.Sprintf("已标记 %d 条通知为已读", n),
		})
		return
	}

	if req.ID > 0 {
		if err := alertRepo.MarkAsRead(req.ID); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"marked_read": 1,
			"id":          req.ID,
		})
		return
	}

	writeError(w, http.StatusBadRequest, "需要指定 id 或 all=true")
}

// HandleAgentDashboard GET /api/agent/dashboard
// Agent 专用仪表盘: 一次返回 alerts + brief + changes 的汇总视图
func (s *Service) HandleAgentDashboard(w http.ResponseWriter, r *http.Request) {
	alertRepo := store.NewAlertRepo(s.db)
	today := time.Now().Format("20060102")

	// 未读通知
	unreadAlerts, _ := alertRepo.GetUnread()

	// 今日通知
	todayAlerts, _ := alertRepo.GetByDate(today)

	// 待处理计划
	planRepo := store.NewPlanRepo(s.db)
	openPlans, _ := planRepo.GetOpenPlans()

	// 任务完成状态
	jobRepo := store.NewJobRepo(s.db)
	taskStatus := map[string]bool{}
	for _, name := range []string{"data_update", "signal", "report", "intraday_monitor", "retention"} {
		done, _ := jobRepo.HasSucceeded(name, today)
		taskStatus[name] = done
	}

	// 数据新鲜度
	lastDate, _ := s.barRepo.GetMaxTradeDate()

	// 辩论结果
	debateRepo := store.NewDebateRepo(s.db)
	todayDebates, _ := debateRepo.GetByDate(lastDate)

	// 决策变更
	var decisionChanges interface{}
	if s.debateOrchestrator != nil && s.debateOrchestrator.IsEnabled() && len(todayDebates) > 0 {
		decisionChanges = s.debateOrchestrator.DetectDecisionChanges(todayDebates)
	} else {
		decisionChanges = []interface{}{}
	}

	// 计划状态汇总
	pending, confirmed, executed := 0, 0, 0
	for _, p := range openPlans {
		switch p.Status {
		case store.PlanStatusPending:
			pending++
		case store.PlanStatusConfirmed:
			confirmed++
		case store.PlanStatusExecuted:
			executed++
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"date":             today,
		"data_last_date":   lastDate,
		"unread_alerts":    unreadAlerts,
		"today_alerts":     todayAlerts,
		"open_plans":       openPlans,
		"today_debates":    todayDebates,
		"decision_changes": decisionChanges,
		"task_completed":   taskStatus,
		"plan_summary": map[string]int{
			"pending":   pending,
			"confirmed": confirmed,
			"executed":  executed,
			"total":     len(openPlans),
		},
	})
}
