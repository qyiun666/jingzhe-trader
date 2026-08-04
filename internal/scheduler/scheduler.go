package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/api"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/report"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// 任务名常量 (job_run 表 / /api/health 健康度展示共用)
const (
	JobDataUpdate = "data_update"
	JobSignal     = "signal"
	JobReconcile  = "reconcile"
	JobReport     = "report"
	JobIntraday   = "intraday_monitor"
	JobRetention  = "retention"
)

// tickInterval 调度检查周期: 每30秒检查一次是否有到点任务
const tickInterval = 30 * time.Second

// Scheduler 内置调度器
// 交易日自动执行: 数据更新 → EOD信号生成 → 对账 → 日报推送 → 数据清理; 盘中定时止损监控
// 标准库 time.Ticker 实现, 所有任务经 runJob wrapper (recover + job_run 记录 + 启动补跑)
type Scheduler struct {
	cfg      *config.Config
	db       *sqlx.DB
	svc      *api.Service
	notifier *notify.FeishuNotifier
	quoteSrc quote.Source

	jobRepo  *store.JobRepo
	planRepo *store.PlanRepo
	calRepo  *store.CalendarRepo

	running      sync.Map  // job_name -> bool, 防止同名任务重叠执行
	lastIntraday time.Time // 上一轮盘中监控时间
	jobWg        sync.WaitGroup // 等待所有 job goroutine 完成 (优雅关闭用)
}

// New 创建调度器
func New(cfg *config.Config, db *sqlx.DB, svc *api.Service) *Scheduler {
	var src quote.Source = quote.NewTencentQuote()
	if cfg.Broker.Type == "qmt" && cfg.Broker.QMT.URL != "" {
		src = quote.NewQMTQuote(cfg.Broker.QMT.URL)
	}
	return &Scheduler{
		cfg:      cfg,
		db:       db,
		svc:      svc,
		notifier: notify.NewFeishuNotifier(cfg.Feishu.WebhookURL),
		quoteSrc: src,
		jobRepo:  store.NewJobRepo(db),
		planRepo: store.NewPlanRepo(db),
		calRepo:  store.NewCalendarRepo(db),
	}
}

// Start 启动调度循环 (阻塞, 由调用方放入 goroutine; ctx 取消后退出)
func (s *Scheduler) Start(ctx context.Context) {
	logger.L().Infow("调度器启动",
		"data_update", s.cfg.Scheduler.DataUpdateTime,
		"signal", s.cfg.Scheduler.SignalTime,
		"report", s.cfg.Scheduler.ReportTime,
		"cleanup", s.cfg.Retention.CleanupTime,
		"intraday_enabled", s.cfg.Scheduler.Intraday.Enabled)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()

	s.tick() // 启动立即检查一次 (重启补跑: 已过时间且当日未成功的任务补跑)
	for {
		select {
		case <-ctx.Done():
			logger.L().Info("调度器等待运行中任务完成后退出...")
			s.jobWg.Wait() // 等待所有 job goroutine 完成, 防止关库时写入半截数据
			logger.L().Info("调度器退出")
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

// tick 单轮调度检查
func (s *Scheduler) tick() {
	now := time.Now()
	today := now.Format("20060102")
	weekday := now.Weekday()

	// 1. 系统时间判断周末: 周六周日直接跳过交易任务 (不依赖数据库)
	if weekday == time.Saturday || weekday == time.Sunday {
		logger.L().Infow("调度器: 今天是周末, 跳过交易任务", "date", today, "weekday", weekday)
		s.maybeRunDaily(JobRetention, s.cfg.Retention.CleanupTime, now, today, func(date string) error {
			return s.runRetention(date, false)
		})
		return
	}

	// 2. 工作日默认是交易日, 只有数据库明确记录为节假日才跳过
	// 这样即使数据库日历过期或缺失, 工作日仍会正常执行任务
	isTradeDay := true // 默认交易日
	cal, err := s.calRepo.GetByDate(today)
	if err != nil {
		logger.L().Warnw("调度器: 查询日历失败, 默认按交易日执行", "date", today, "err", err)
	} else if cal != nil {
		isTradeDay = cal.IsOpen == 1
	} else {
		// 数据库没有今天的记录 -> 默认按交易日执行, 后台异步同步日历(不阻塞任务)
		logger.L().Infow("调度器: 日历数据缺失, 默认按交易日执行, 后台同步日历", "date", today)
		go func() {
			if err := s.svc.SyncCalendar(); err != nil {
				logger.L().Warnw("调度器: 后台同步日历失败", "err", err)
			} else {
				logger.L().Info("调度器: 后台日历同步完成")
			}
		}()
	}

	if isTradeDay {
		s.maybeRunDaily(JobDataUpdate, s.cfg.Scheduler.DataUpdateTime, now, today, s.runDataUpdate)
		s.maybeRunDaily(JobSignal, s.cfg.Scheduler.SignalTime, now, today, s.runSignal)
		if s.cfg.Broker.Type == "qmt" {
			s.maybeRunDaily(JobReconcile, reconcileTime(s.cfg.Scheduler.SignalTime), now, today, s.runReconcile)
		}
		s.maybeRunDaily(JobReport, s.cfg.Scheduler.ReportTime, now, today, s.runReport)
		s.maybeRunIntraday(now, today)
	} else {
		logger.L().Infow("调度器: 今天是节假日(数据库确认), 跳过交易任务", "date", today)
	}
	// 数据清理每日执行 (非交易日仅做文件清理与WAL瘦身)
	s.maybeRunDaily(JobRetention, s.cfg.Retention.CleanupTime, now, today, func(date string) error {
		return s.runRetention(date, isTradeDay)
	})
}

// maybeRunDaily 到点且当日未成功执行时触发每日任务 (重启后补跑一次)
func (s *Scheduler) maybeRunDaily(name, at string, now time.Time, today string, fn func(date string) error) {
	scheduled, err := parseClock(at, now)
	if err != nil {
		logger.L().Warnw("调度器: 任务时间配置无效", "job", name, "at", at)
		return
	}
	if now.Before(scheduled) {
		return
	}
	if done, err := s.jobRepo.HasSucceeded(name, today); err != nil || done {
		return
	}
	s.runJob(name, today, fn)
}

// maybeRunIntraday 盘中止损监控: 交易时段内每 interval_min 分钟一轮
func (s *Scheduler) maybeRunIntraday(now time.Time, today string) {
	ic := s.cfg.Scheduler.Intraday
	if !ic.Enabled {
		return
	}
	start, err1 := parseClock(ic.Start, now)
	end, err2 := parseClock(ic.End, now)
	if err1 != nil || err2 != nil || now.Before(start) || now.After(end) {
		return
	}
	interval := time.Duration(ic.IntervalMin) * time.Minute
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if now.Sub(s.lastIntraday) < interval {
		return
	}
	s.lastIntraday = now
	s.runJob(JobIntraday, today, s.runIntradayMonitor)
}

// runJob 统一任务执行 wrapper: 互斥防重叠 + recover隔离 + job_run 记录 + 失败飞书告警
func (s *Scheduler) runJob(name, date string, fn func(date string) error) {
	if _, loaded := s.running.LoadOrStore(name, true); loaded {
		logger.L().Warnw("调度器: 上一轮任务未结束, 跳过本轮", "job", name)
		return
	}

	s.jobWg.Add(1)
	go func() {
		defer s.jobWg.Done()
		defer s.running.Delete(name)

		jobID, err := s.jobRepo.StartJob(name, date)
		if err != nil {
			logger.L().Errorw("调度器: 记录任务开始失败", "job", name, "err", err)
		}

		defer func() {
			if rec := recover(); rec != nil {
				msg := fmt.Sprintf("任务 %s panic: %v", name, rec)
				logger.L().Errorw("调度器: 任务panic", "job", name, "panic", rec, "stack", string(debug.Stack()))
				s.finishJob(jobID, store.JobStatusFailed, msg)
				s.alert("⚠️ 惊蛰调度任务崩溃", msg)
			}
		}()

		logger.L().Infow("调度器: 任务开始", "job", name, "date", date)
		if err := fn(date); err != nil {
			logger.L().Errorw("调度器: 任务失败", "job", name, "err", err)
			s.finishJob(jobID, store.JobStatusFailed, err.Error())
			s.alert("⚠️ 惊蛰调度任务失败", fmt.Sprintf("任务 %s (%s) 失败: %v", name, date, err))
			return
		}
		s.finishJob(jobID, store.JobStatusSuccess, "")
		logger.L().Infow("调度器: 任务完成", "job", name, "date", date)
	}()
}

// finishJob 记录任务结束 (jobID<=0 时跳过)
func (s *Scheduler) finishJob(jobID int64, status, errMsg string) {
	if jobID <= 0 {
		return
	}
	if err := s.jobRepo.FinishJob(jobID, status, errMsg); err != nil {
		logger.L().Errorw("调度器: 记录任务结束失败", "job_id", jobID, "err", err)
	}
}

// alert 飞书告警 + 落库 (Agent 可通过 /api/agent/alerts 读取)
// 降级: 飞书发送失败只打日志, 不影响落库
func (s *Scheduler) alert(title, text string) {
	// 1. 落库 (无论飞书是否配置, 都存一份供 Agent 读取)
	alertRepo := store.NewAlertRepo(s.db) // Scheduler 独立实例 (与 Service.alertRepo 不同生命周期)
	level := store.AlertLevelInfo
	jobName := ""
	today := time.Now().Format("20060102")

	// 根据标题推断级别和来源
	switch {
	case strings.Contains(title, "🚨") || strings.Contains(title, "崩溃") || strings.Contains(title, "失败"):
		level = store.AlertLevelUrgent
	case strings.Contains(title, "⚠️"):
		level = store.AlertLevelWarning
	case strings.Contains(title, "✅"):
		level = store.AlertLevelSuccess
	}

	// 从标题提取 job_name
	for _, name := range []string{"信号", "日报", "对账", "止损", "计划", "提醒"} {
		if strings.Contains(title, name) {
			switch name {
			case "信号":
				jobName = "signal"
			case "日报":
				jobName = "report"
			case "对账":
				jobName = "reconcile"
			case "止损":
				jobName = "intraday_monitor"
			case "计划":
				jobName = "signal"
			case "提醒":
				jobName = "report"
			}
			break
		}
	}

	alert := &store.AgentAlert{
		TradeDate: today,
		JobName:   jobName,
		Level:     level,
		Title:     title,
		Content:   text,
	}
	if _, err := alertRepo.Insert(alert); err != nil {
		logger.L().Warnw("通知落库失败", "title", title, "err", err)
	}

	// 2. 飞书发送 (失败不影响流程)
	if err := s.notifier.SendText(title + "\n" + text); err != nil {
		logger.L().Warnw("飞书告警发送失败", "err", err)
	}
}

// ==================== 具体任务实现 ====================

// runDataUpdate 15:10 数据更新 (进程内调用 dataloader)
func (s *Scheduler) runDataUpdate(date string) error {
	return s.svc.UpdateData()
}

// runSignal 15:30 EOD 信号生成 → 次日交易计划落库
// 增强通知: 无论有无计划都通知用户, 包含决策变更和状态汇总
func (s *Scheduler) runSignal(date string) error {
	plans, err := s.svc.GenerateTradePlans(date)
	if err != nil {
		return fmt.Errorf("生成交易计划失败: %w", err)
	}
	// 旧的 pending 计划过期, 再写入当日新计划
	if n, err := s.planRepo.ExpireOldPlans(date); err != nil {
		logger.L().Warnw("过期旧计划失败", "err", err)
	} else if n > 0 {
		logger.L().Infow("过期旧计划", "count", n)
	}
	if err := s.planRepo.ReplaceDayPlans(date, plans); err != nil {
		return fmt.Errorf("交易计划落库失败: %w", err)
	}
	logger.L().Infow("EOD交易计划生成完成", "date", date, "count", len(plans))

	// 增强通知: 无论有无计划都通知
	s.notifySignalResult(date, plans)
	return nil
}

// notifySignalResult 发送信号生成结果通知 (每次都通知, 包含决策变更检测)
func (s *Scheduler) notifySignalResult(date string, plans []*store.TradePlan) {
	var lines []string

	// 计划汇总
	buyCount := 0
	sellCount := 0
	urgentCount := 0
	for _, p := range plans {
		if p.Direction == "buy" {
			buyCount++
		} else {
			sellCount++
		}
		if p.Urgency == store.PlanUrgencyUrgent {
			urgentCount++
		}
	}

	if len(plans) == 0 {
		lines = append(lines, fmt.Sprintf("📅 %s 信号生成完成: 今日无交易计划", date))
		lines = append(lines, "策略未触发买卖信号, 继续持有当前仓位")
	} else {
		summary := fmt.Sprintf("📋 %s 信号生成完成: 共%d条计划 (买入%d/卖出%d", date, len(plans), buyCount, sellCount)
		if urgentCount > 0 {
			summary += fmt.Sprintf(", 紧急%d", urgentCount)
		}
		summary += ")"
		lines = append(lines, summary)
		// 列出前5条计划
		showCount := len(plans)
		if showCount > 5 {
			showCount = 5
		}
		for i := 0; i < showCount; i++ {
			p := plans[i]
			icon := "🟢"
			if p.Direction == "sell" {
				icon = "🔴"
			}
			if p.Urgency == store.PlanUrgencyUrgent {
				icon = "🚨"
			}
			lines = append(lines, fmt.Sprintf("%s %s %s %d股 @%.2f", icon, p.Name, p.Direction, p.Qty, p.RefPrice))
		}
		if len(plans) > 5 {
			lines = append(lines, fmt.Sprintf("... 还有%d条, 详见 /api/plan", len(plans)-5))
		}
	}

	// 决策变更检测
	if todayDebates, err := store.NewDebateRepo(s.db).GetByDate(date); err == nil && len(todayDebates) > 0 {
		if s.svc.DebateOrchestrator() != nil && s.svc.DebateOrchestrator().IsEnabled() {
			changes := s.svc.DebateOrchestrator().DetectDecisionChanges(todayDebates)
			if len(changes) > 0 {
				lines = append(lines, "")
				lines = append(lines, fmt.Sprintf("🔄 决策变更检测: %d个标的决策发生变化", len(changes)))
				showChanges := len(changes)
				if showChanges > 3 {
					showChanges = 3
				}
				for i := 0; i < showChanges; i++ {
					lines = append(lines, fmt.Sprintf("  - %s: %s", changes[i].Name, changes[i].Detail))
				}
				if len(changes) > 3 {
					lines = append(lines, fmt.Sprintf("  ... 还有%d项变更", len(changes)-3))
				}
			}
		}
	}

	// 待处理计划提醒
	if openPlans, err := s.planRepo.GetOpenPlans(); err == nil {
		pending := 0
		confirmed := 0
		for _, p := range openPlans {
			if p.Status == store.PlanStatusPending {
				pending++
			}
			if p.Status == store.PlanStatusConfirmed {
				confirmed++
			}
		}
		if pending > 0 || confirmed > 0 {
			lines = append(lines, "")
			if pending > 0 {
				lines = append(lines, fmt.Sprintf("⏸️ 待确认: %d条 (请审阅 /api/plan)", pending))
			}
			if confirmed > 0 {
				lines = append(lines, fmt.Sprintf("⏳ 待反馈: %d条 (已确认等待成交反馈)", confirmed))
			}
		}
	}

	// 发送通知
	msg := ""
	for i, l := range lines {
		if i > 0 {
			msg += "\n"
		}
		msg += l
	}
	s.alert("📋 惊蛰交易信号", msg)
}

// runReconcile 15:35 对账 (仅 QMT 实盘): 本地记录 vs 券商
func (s *Scheduler) runReconcile(date string) error {
	brk := broker.NewQMTBridge(s.cfg.Broker.QMT.URL)
	result, err := report.Reconcile(brk, store.NewTradeRepo(s.db), date)
	if err != nil {
		return fmt.Errorf("对账执行失败: %w", err)
	}
	if !result.IsBalanced {
		s.alert("⚠️ 惊蛰对账差异", report.GenerateReconcileReport(result))
	}
	return nil
}

// runReport 15:45 日报生成 + 飞书推送
// 增强通知: 日报推送后追加操作提醒
func (s *Scheduler) runReport(date string) error {
	daily, err := s.svc.RunDaily(date, s.svc.SelectStrategy(date))
	if err != nil {
		return fmt.Errorf("生成日报失败: %w", err)
	}
	if !s.notifier.Enabled() {
		logger.L().Info("飞书未配置, 日报仅落库不推送")
		return nil
	}
	if err := s.notifier.SendCard(api.BuildFeishuDailyCard(daily)); err != nil {
		return fmt.Errorf("推送日报失败: %w", err)
	}

	// 追加操作提醒: 检查是否有待处理计划
	if openPlans, err := s.planRepo.GetOpenPlans(); err == nil && len(openPlans) > 0 {
		pending := 0
		confirmed := 0
		for _, p := range openPlans {
			if p.Status == store.PlanStatusPending {
				pending++
			}
			if p.Status == store.PlanStatusConfirmed {
				confirmed++
			}
		}
		if pending > 0 || confirmed > 0 {
			reminder := fmt.Sprintf("📊 日报已推送, 请查看:\n")
			if pending > 0 {
				reminder += fmt.Sprintf("⏸️ %d条计划待确认 (POST /api/plan/confirm)\n", pending)
			}
			if confirmed > 0 {
				reminder += fmt.Sprintf("⏳ %d条计划待执行反馈 (POST /api/trade/confirm)\n", confirmed)
			}
			reminder += "完整数据: GET /api/agent/brief | 变更检测: GET /api/agent/changes"
			s.alert("📌 惊蛰操作提醒", reminder)
		}
	}
	return nil
}

// runIntradayMonitor 盘中止损监控: 实时价 → 止损检查 → 紧急卖出计划 + 飞书告警
func (s *Scheduler) runIntradayMonitor(date string) error {
	positions, err := store.NewPortfolioRepo(s.db).GetAllPositions()
	if err != nil {
		return fmt.Errorf("查询持仓失败: %w", err)
	}
	if len(positions) == 0 {
		return nil
	}

	codes := make([]string, 0, len(positions))
	for _, p := range positions {
		codes = append(codes, p.TsCode)
	}
	prices, err := s.quoteSrc.GetRealtimePrices(codes)
	if err != nil {
		return fmt.Errorf("拉取实时行情失败: %w", err)
	}

	// 已有未处理紧急计划的股票不重复告警
	existing := map[string]bool{}
	if plans, err := s.planRepo.GetPlansByDate(date); err == nil {
		for _, p := range plans {
			if p.Urgency == store.PlanUrgencyUrgent && p.Status == store.PlanStatusPending {
				existing[p.TsCode] = true
			}
		}
	}

	sl := risk.NewStopLossManager(s.cfg.Risk.StopLossPct, s.cfg.Risk.TakeProfitPct)
	for _, p := range positions {
		price, ok := prices[p.TsCode]
		if !ok || existing[p.TsCode] {
			continue
		}
		pos := &model.Position{
			TsCode:       p.TsCode,
			TotalQty:     p.TotalQty,
			AvailableQty: p.AvailableQty,
			CostPrice:    p.CostPrice,
		}
		triggered, reason := sl.CheckSingle(pos, price)
		if !triggered {
			continue
		}
		qty := p.AvailableQty
		if qty <= 0 {
			qty = p.TotalQty // T+1 不可卖也先生成计划提示
		}
		plan := &store.TradePlan{
			TradeDate: date,
			TsCode:    p.TsCode,
			Direction: "sell",
			Qty:       qty,
			RefPrice:  price,
			Reason:    "盘中" + reason,
			Strategy:  "stop_loss",
			Urgency:   store.PlanUrgencyUrgent,
		}
		if _, err := s.planRepo.InsertPlan(plan); err != nil {
			logger.L().Errorw("紧急计划落库失败", "ts_code", p.TsCode, "err", err)
			continue
		}
		s.alert("🚨 惊蛰盘中止损告警",
			fmt.Sprintf("%s 现价 %.2f 触发: %s\n已生成紧急卖出计划(%d股), 请尽快确认", p.TsCode, price, reason, qty))
	}
	return nil
}

// runRetention 16:30 数据保留清理 + SQLite 瘦身
func (s *Scheduler) runRetention(date string, fullClean bool) error {
	rc := s.cfg.Retention
	logDir := ""
	if s.cfg.Log.FilePath != "" {
		logDir = filepath.Dir(s.cfg.Log.FilePath)
	}
	return store.RunRetention(s.db, store.RetentionPolicy{
		BarYears:     rc.BarYears,
		NewsDays:     rc.NewsDays,
		PlanDays:     rc.PlanDays,
		BacktestRuns: rc.BacktestRuns,
		LogDays:      rc.LogDays,
		ReportFiles:  rc.ReportFiles,
		LogDir:       logDir,
		ReportDir:    "reports",
	}, fullClean)
}

// ==================== 时间工具 ====================

// parseClock 将 "HH:MM" 解析为当天的时间点
func parseClock(clock string, now time.Time) (time.Time, error) {
	t, err := time.Parse("15:04", clock)
	if err != nil {
		return time.Time{}, fmt.Errorf("时间格式无效(%s): %w", clock, err)
	}
	return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location()), nil
}

// reconcileTime 对账时间 = 信号时间 + 5 分钟 (默认 15:35)
func reconcileTime(signalAt string) string {
	t, err := time.Parse("15:04", signalAt)
	if err != nil {
		return "15:35"
	}
	return t.Add(5 * time.Minute).Format("15:04")
}
