package api

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"jingzhe-trader/internal/agent"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// ==================== Agent 专用接口 ====================

// AgentBrief Agent 单次调用所需的全量上下文
type AgentBrief struct {
	Date              string                 `json:"date"`                // 数据基准日期
	DataLastDate      string                 `json:"data_last_date"`      // 数据库最新行情日期
	DataFresh         bool                   `json:"data_fresh"`          // 数据是否新鲜
	OpenPlans         []store.TradePlan      `json:"open_plans"`          // 待处理的交易计划
	Portfolio         *PortfolioJSON         `json:"portfolio"`           // 持仓诊断
	Market            *MarketSnapshotJSON    `json:"market"`              // 市场概况
	Jobs              map[string]string      `json:"jobs"`                // 各任务最近成功时间
	Warnings          []string               `json:"warnings"`            // 数据/任务异常提示
	Debates           []store.DebateResult   `json:"debates"`             // 当日辩论结果
	ScreenResults     []store.ScreenResult   `json:"screen_results"`      // 最新选股结果
	ActionNeeded      []string               `json:"action_needed"`       // 需要用户操作的提示
	DecisionChanges   []agent.DecisionChange `json:"decision_changes"`    // 决策变更记录
	PlanStatusSummary PlanStatusSummary      `json:"plan_status_summary"` // 交易计划状态汇总
	TaskCompleted     map[string]bool        `json:"task_completed"`      // 当日各任务是否已完成
	Goal              *goal.Status           `json:"goal,omitempty"`      // 季度目标状态 (目标跟踪启用时)
}

// PlanStatusSummary 交易计划状态汇总
type PlanStatusSummary struct {
	Pending   int `json:"pending"`   // 待确认
	Confirmed int `json:"confirmed"` // 已确认待执行
	Executed  int `json:"executed"`  // 已执行
	Expired   int `json:"expired"`   // 已过期
	Total     int `json:"total"`     // 总计
}

// BuildAgentBrief builds the full Agent context without writing an HTTP response.
func (s *Service) BuildAgentBrief() *AgentBrief {
	brief := &AgentBrief{Jobs: map[string]string{}, Warnings: []string{}}

	// 数据新鲜度
	lastDate, err := s.barRepo.GetMaxTradeDate()
	if err != nil || lastDate == "" {
		brief.Warnings = append(brief.Warnings, "数据库无行情数据, 请先执行数据更新")
		return brief
	}
	brief.Date = lastDate
	brief.DataLastDate = lastDate
	today := time.Now().Format("20060102")
	if preDate, perr := s.calRepo.GetPreTradeDate(today); perr == nil && preDate != "" {
		brief.DataFresh = lastDate >= preDate
	}
	if !brief.DataFresh {
		brief.Warnings = append(brief.Warnings, "行情数据不是最新, 计划参考价可能过期")
	}

	// 待处理计划
	if plans, perr := s.planRepo.GetOpenPlans(); perr == nil {
		brief.OpenPlans = plans
	} else {
		brief.Warnings = append(brief.Warnings, "查询交易计划失败: "+perr.Error())
	}

	// 辩论结果
	if debates, derr := s.debateRepo.GetByDate(lastDate); derr == nil {
		brief.Debates = debates
	}

	// 选股结果
	if s.screenRepo != nil {
		if screenResults, err := s.screenRepo.GetLatest(); err == nil {
			brief.ScreenResults = screenResults
		}
	}

	// 持仓诊断与市场概况 (尽力而为)
	if portfolio, perr := s.RunPositions(lastDate); perr == nil {
		brief.Portfolio = portfolio
	}
	if market, merr := s.RunMarket(lastDate); merr == nil {
		brief.Market = market
	}

	// 季度目标状态 (Agent 决策的核心约束之一)
	if s.goalTracker != nil {
		if st, gerr := s.GoalStatus(lastDate); gerr == nil {
			brief.Goal = st
			if st.Mode != goal.ModeNormal {
				brief.Warnings = append(brief.Warnings, fmt.Sprintf("目标风控模式: %s — %s", st.ModeLabel, strings.Join(st.Notes, "; ")))
			}
		}
	}

	// 任务健康度 (遍历全部注册任务, 避免硬编码遗漏新增任务)
	for _, name := range store.JobNames {
		if run, jerr := s.jobRepo.LastSuccess(name); jerr == nil && run != nil {
			brief.Jobs[name] = run.FinishedAt
		}
	}

	// 决策变更检测
	if s.debateOrchestrator != nil && s.debateOrchestrator.IsEnabled() && len(brief.Debates) > 0 {
		brief.DecisionChanges = s.debateOrchestrator.DetectDecisionChanges(brief.Debates)
	}

	// 交易计划状态汇总
	brief.PlanStatusSummary = s.buildPlanStatusSummary(brief.OpenPlans)

	// 当日任务完成状态
	brief.TaskCompleted = s.buildTaskCompletedStatus(today)

	// 需要用户操作的提示
	brief.ActionNeeded = s.buildActionNeededEnhanced(brief.OpenPlans, brief.DecisionChanges, brief.PlanStatusSummary)

	return brief
}

// buildPlanStatusSummary 构建交易计划状态汇总
func (s *Service) buildPlanStatusSummary(plans []store.TradePlan) PlanStatusSummary {
	summary := PlanStatusSummary{Total: len(plans)}
	for _, p := range plans {
		switch p.Status {
		case store.PlanStatusPending:
			summary.Pending++
		case store.PlanStatusConfirmed:
			summary.Confirmed++
		case store.PlanStatusExecuted:
			summary.Executed++
		case store.PlanStatusExpired:
			summary.Expired++
		}
	}
	return summary
}

// buildTaskCompletedStatus 构建当日任务完成状态
func (s *Service) buildTaskCompletedStatus(today string) map[string]bool {
	status := map[string]bool{}
	for _, name := range store.JobNames {
		done, err := s.jobRepo.HasSucceeded(name, today)
		if err == nil {
			status[name] = done
		} else {
			status[name] = false
		}
	}
	return status
}

// buildActionNeededEnhanced 增强版操作提示 (包含决策变更)
func (s *Service) buildActionNeededEnhanced(plans []store.TradePlan, changes []agent.DecisionChange, summary PlanStatusSummary) []string {
	var actions []string

	// 待确认计划
	if summary.Pending > 0 {
		actions = append(actions, fmt.Sprintf("📋 有%d条待确认的交易计划，请审阅后确认或忽略", summary.Pending))
	}

	// 已确认待执行
	if summary.Confirmed > 0 {
		actions = append(actions, fmt.Sprintf("⏳ 有%d条已确认计划等待执行反馈，请在券商成交后反馈", summary.Confirmed))
	}

	// 决策变更
	changeCount := len(changes)
	if changeCount > 0 {
		actions = append(actions, fmt.Sprintf("🔄 检测到%d个标的投资决策发生变化，请关注", changeCount))
	}

	// 无操作提示
	if len(actions) == 0 {
		actions = append(actions, "✅ 当前无需操作，系统运行正常")
	}

	return actions
}

// BuildPlans returns trade plans for a date, or all open plans if date is empty.
func (s *Service) BuildPlans(date string) []store.TradePlan {
	var plans []store.TradePlan
	var err error
	if date != "" {
		plans, err = s.planRepo.GetPlansByDate(strings.ReplaceAll(strings.TrimSpace(date), "-", ""))
	} else {
		plans, err = s.planRepo.GetOpenPlans()
	}
	if err != nil {
		return []store.TradePlan{}
	}
	return plans
}

// PlanConfirmRequest 计划确认请求
type PlanConfirmRequest struct {
	ID int64 `json:"id"` // 交易计划ID
}

// ConfirmPlan confirms a trade plan by ID and returns the updated plan.
func (s *Service) ConfirmPlan(id int64) (*store.TradePlan, error) {
	plan, err := s.planRepo.GetPlanByID(id)
	if err != nil {
		return nil, err
	}
	if plan.Status != store.PlanStatusPending {
		return nil, fmt.Errorf("计划状态为 %s, 无法确认", plan.Status)
	}

	status := store.PlanStatusConfirmed
	// 自动执行: 仅 QMT 实盘模式下真实下单; 下单结果邮件推送
	if s.cfg.Trading.AutoExecute && s.cfg.Broker.Type == "qmt" {
		mailer := notify.NewMailNotifier(s.cfg.Mail.Enabled, s.cfg.Mail.From, s.cfg.Mail.Password)
		if err := s.executePlanViaQMT(plan); err != nil {
			msg := fmt.Sprintf("%s %s %d股 @%.2f: %v", plan.TsCode, plan.Direction, plan.Qty, plan.RefPrice, err)
			if nerr := mailer.Send("❌ 惊蛰下单失败", msg); nerr != nil {
				logger.L().Warnw("邮件通知发送失败", "err", nerr)
			}
			return nil, fmt.Errorf("QMT下单失败: %w", err)
		}
		msg := fmt.Sprintf("%s %s %d股 @%.2f (%s)", plan.TsCode, plan.Direction, plan.Qty, plan.RefPrice, plan.Reason)
		if nerr := mailer.Send("✅ 惊蛰下单成功", msg); nerr != nil {
			logger.L().Warnw("邮件通知发送失败", "err", nerr)
		}
		status = store.PlanStatusExecuted
	}

	if err := s.planRepo.UpdatePlanStatus(plan.ID, status); err != nil {
		return nil, err
	}
	plan.Status = status
	return plan, nil
}

// executePlanViaQMT 通过 QMT 桥执行交易计划, 成功后同步本地持仓
// 复用组合根注入的 broker 实例 (禁止内部 new, 且新建实例会丢失 OnTrade 回调)
func (s *Service) executePlanViaQMT(plan *store.TradePlan) error {
	bridge, ok := s.brk.(*broker.QMTBridge)
	if !ok {
		return fmt.Errorf("broker 类型为 %T, 非 QMT 实盘模式, 拒绝自动下单", s.brk)
	}
	side := model.SideBuy
	if plan.Direction == "sell" {
		side = model.SideSell
	}
	if _, err := bridge.PlaceOrder(broker.OrderRequest{
		TsCode:   plan.TsCode,
		Side:     side,
		Qty:      plan.Qty,
		Price:    plan.RefPrice,
		Reason:   plan.Reason,
		Strategy: plan.Strategy,
	}); err != nil {
		return fmt.Errorf("下单失败: %w", err)
	}
	// 同步本地持仓记录 (与 /api/trade/confirm 同一逻辑); 失败必须报错, 防止内存与 DB 持仓不一致
	if _, err := s.applyTradeToPortfolio(plan.TsCode, side, plan.Qty, plan.RefPrice); err != nil {
		return fmt.Errorf("QMT成交同步持仓失败: %w", err)
	}
	return nil
}

// ==================== 健康检查扩展 ====================

// HealthStatus /api/health 响应
type HealthStatus struct {
	Status       string            `json:"status"`
	Uptime       string            `json:"uptime"`
	Goroutines   int               `json:"goroutines"`
	DBSizeBytes  int64             `json:"db_size_bytes"`
	DataLastDate string            `json:"data_last_date"`
	DataFresh    bool              `json:"data_fresh"`
	Jobs         map[string]string `json:"jobs"` // 各任务最近成功时间
}

// BuildHealthStatus 构建健康状态 (含运行时指标与任务健康度)
func (s *Service) BuildHealthStatus() *HealthStatus {
	hs := &HealthStatus{
		Status:     "ok",
		Uptime:     time.Since(s.startTime).Truncate(time.Second).String(),
		Goroutines: runtime.NumGoroutine(),
		Jobs:       map[string]string{},
	}
	if err := s.db.Ping(); err != nil {
		hs.Status = "db_error"
		return hs
	}
	// 库文件路径取自连接自身: 配置本体就存在这个库里, 路径不可能再由配置给出
	var dbFile string
	if err := s.db.Get(&dbFile, `SELECT file FROM pragma_database_list WHERE name='main'`); err == nil {
		if info, statErr := os.Stat(dbFile); statErr == nil {
			hs.DBSizeBytes = info.Size()
		}
	}
	if lastDate, err := s.barRepo.GetMaxTradeDate(); err == nil {
		hs.DataLastDate = lastDate
		today := time.Now().Format("20060102")
		if preDate, perr := s.calRepo.GetPreTradeDate(today); perr == nil && preDate != "" {
			hs.DataFresh = lastDate >= preDate
		}
	}
	jobRepo := store.NewJobRepo(s.db)
	for _, name := range store.JobNames {
		if run, err := jobRepo.LastSuccess(name); err == nil && run != nil {
			hs.Jobs[name] = run.FinishedAt
		}
	}
	return hs
}
