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
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/report"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// 任务名常量 (job_run 表 / /api/health 健康度展示共用)
const (
	JobDataUpdate = "data_update"
	JobScreener   = "screener"
	JobSignal     = "signal"
	JobReconcile  = "reconcile"
	JobReport     = "report"
	JobIntraday   = "intraday_monitor"
	JobRetention  = "retention"
	JobSettleT1   = "settle_t1"
)

// settleT1Time T+1 持仓结转时间 (开盘前, 盘中监控启动前)
const settleT1Time = "09:25"

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

	running      sync.Map       // job_name -> bool, 防止同名任务重叠执行
	lastIntraday time.Time      // 上一轮盘中监控时间
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
		s.maybeRunDaily(JobSettleT1, settleT1Time, now, today, s.runSettleT1)
		s.maybeRunDataUpdateWithRetry(now, today)
		s.maybeRunDaily(JobScreener, s.cfg.Scheduler.ScreenerTime, now, today, s.runScreener)
		s.maybeRunDaily(JobSignal, s.cfg.Scheduler.SignalTime, now, today, s.runSignalWithFreshnessCheck)
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

// maybeRunDataUpdateWithRetry 数据更新支持多重试时间
// 首次在 data_update_time 执行, 失败后在 signal_time 和 signal_time+30min 自动重试
// 重试间隔通过 job_run 表的上次尝试时间判断, 避免同一窗口内重复执行
func (s *Scheduler) maybeRunDataUpdateWithRetry(now time.Time, today string) {
	if done, err := s.jobRepo.HasSucceeded(JobDataUpdate, today); err != nil || done {
		return
	}

	// 重试时间点: 15:10 (首次) → 15:30 (信号前) → 16:00 (报告后)
	retryTimes := []string{s.cfg.Scheduler.DataUpdateTime}
	if s.cfg.Scheduler.SignalTime != "" && s.cfg.Scheduler.SignalTime != s.cfg.Scheduler.DataUpdateTime {
		retryTimes = append(retryTimes, s.cfg.Scheduler.SignalTime)
	}
	if t, err := time.Parse("15:04", s.cfg.Scheduler.SignalTime); err == nil {
		third := t.Add(30 * time.Minute).Format("15:04")
		retryTimes = append(retryTimes, third)
	}

	for _, at := range retryTimes {
		scheduled, err := parseClock(at, now)
		if err != nil || now.Before(scheduled) {
			continue
		}
		// 上次尝试在该重试时间之前 → 可以重试; 之后 → 跳过 (本窗口已尝试过)
		lastAttempt, _ := s.jobRepo.LastAttemptStartedAt(JobDataUpdate, today)
		if lastAttempt.IsZero() || lastAttempt.Before(scheduled) {
			s.runJob(JobDataUpdate, today, s.runDataUpdate)
			return
		}
	}
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
	if err := s.svc.UpdateData(); err != nil {
		return err
	}
	// 数据新鲜后记录实盘账户快照 (收益曲线; 失败不阻断主任务)
	if err := s.svc.RecordLiveSnapshot(date); err != nil {
		logger.L().Warnw("实盘账户快照记录失败", "date", date, "err", err)
	}
	// 季度目标评估: 风险模式变化时告警 (失败不阻断)
	s.checkGoalMode(date)
	return nil
}

// checkGoalMode 评估季度目标状态, 风险模式变化时飞书告警并记录
func (s *Scheduler) checkGoalMode(date string) {
	st, err := s.svc.GoalStatus(date)
	if err != nil || st == nil {
		return // 未启用目标跟踪
	}
	portRepo := store.NewPortfolioRepo(s.db)
	lastMode, _ := portRepo.GetMeta("goal_risk_mode")
	if st.Mode == lastMode {
		return
	}
	portRepo.SetMeta("goal_risk_mode", st.Mode)
	logger.L().Infow("季度目标风险模式切换", "date", date, "from", lastMode, "to", st.Mode,
		"return", st.ReturnPct, "progress", st.Progress, "drawdown", st.DrawdownPct)
	s.alert("🎯 惊蛰目标跟踪", fmt.Sprintf(
		"%s 风险模式: %s → %s\n季度收益: %.2f%% (目标 %.1f%%, 进度 %.0f%%)\n回撤: %.2f%% (预算 %.1f%%, 消耗 %.0f%%)\n%s",
		st.Quarter, goal.ModeLabel(lastMode), st.ModeLabel,
		st.ReturnPct*100, st.TargetPct*100, st.Progress*100,
		st.DrawdownPct*100, st.BudgetPct*100, st.BudgetConsumed*100,
		strings.Join(st.Notes, "; ")))
}

// runScreener 15:15 自动选股 (数据更新后, 信号生成前)
// 全市场筛选候选股票 → 同步历史K线 → 结果落库 → 通知
func (s *Scheduler) runScreener(date string) error {
	if s.svc.Screener() == nil {
		return nil
	}
	results, err := s.svc.Screener().Screen(date)
	if err != nil {
		return fmt.Errorf("选股失败: %w", err)
	}

	// 通知
	var lines []string
	if len(results) == 0 {
		lines = append(lines, fmt.Sprintf("📅 %s 全市场选股完成: 无符合条件的候选股票", date))
	} else {
		lines = append(lines, fmt.Sprintf("🔍 %s 全市场选股完成: 筛选出 %d 只候选股票", date, len(results)))
		lines = append(lines, "前5名:")
		for i, c := range results {
			if i >= 5 {
				break
			}
			lines = append(lines, fmt.Sprintf("  #%d %s %s  收盘%.2f 涨跌%.1f%% 换手%.1f%% PE=%.0f 评分%.1f",
				i+1, c.TsCode, c.Name, c.Close, c.PctChg, c.TurnoverRate, c.PE_TTM, c.Score))
		}
		lines = append(lines, "候选股票已自动加入策略股票池, 15:30 信号生成时将一并扫描")
	}
	s.alert("🔍 惊蛰自动选股", strings.Join(lines, "\n"))

	logger.L().Infow("选股任务完成", "date", date, "candidates", len(results))
	return nil
}

// runSignalWithFreshnessCheck 信号生成前检查数据新鲜度, 不新鲜则阻塞等待数据更新完成
// 场景: 15:10 数据更新失败 (Tushare 延迟), 15:30 信号生成时自动补拉
func (s *Scheduler) runSignalWithFreshnessCheck(date string) error {
	barRepo := store.NewBarRepo(s.db)
	maxDate, err := barRepo.GetMaxTradeDate()
	if err != nil {
		// 无法确认数据新鲜度时宁缺毋滥: 不用未知状态的数据做交易决策
		s.alert("🚨 惊蛰信号中止", fmt.Sprintf("查询数据新鲜度失败: %v, 今日信号生成中止, 请检查数据库", err))
		return fmt.Errorf("查询数据新鲜度失败, 中止信号生成: %w", err)
	}
	if maxDate < date {
		logger.L().Infow("信号任务: 检测到数据不新鲜, 触发阻塞式数据更新", "latest_data", maxDate, "expected", date)
		s.alert("📅 惊蛰数据更新", fmt.Sprintf(
			"信号生成前检测到数据不新鲜 (库内最新: %s, 期望: %s), 自动触发数据更新",
			maxDate, date))
		if err := s.svc.UpdateDataBlocking(); err != nil {
			// 硬失败: 陈旧数据生成的计划是错误决策的来源, 宁可今日无计划
			s.alert("🚨 惊蛰信号中止", fmt.Sprintf("数据更新失败: %v, 今日信号生成中止 (不用陈旧数据做决策)", err))
			return fmt.Errorf("前置数据更新失败, 中止信号生成: %w", err)
		}
		// 更新成功后再次校验: Tushare 延迟时更新"成功"也可能没有当日数据
		if maxDate2, err := barRepo.GetMaxTradeDate(); err != nil || maxDate2 < date {
			s.alert("🚨 惊蛰信号中止", fmt.Sprintf(
				"数据更新后仍无当日数据 (库内最新: %s, 期望: %s), 今日信号生成中止, 请检查数据源", maxDate2, date))
			return fmt.Errorf("数据更新后仍不新鲜 (最新: %s, 期望: %s), 中止信号生成", maxDate2, date)
		}
		logger.L().Info("信号任务: 前置数据更新完成")
	} else {
		logger.L().Infow("信号任务: 数据新鲜度检查通过", "latest_data", maxDate)
	}
	return s.runSignal(date)
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
	// 成交回报轮询: 把券商端真实成交落库 (OnTrade 回调 → trades 表, run_id=live)
	if err := brk.PollTrades(); err != nil {
		logger.L().Warnw("成交回报轮询失败", "date", date, "err", err)
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

// runSettleT1 每日 09:25 T+1 持仓结转 (昨日买入转为可卖)
func (s *Scheduler) runSettleT1(date string) error {
	if err := s.svc.SettleT1(date); err != nil {
		s.alert("⚠️ 惊蛰T+1结转失败", fmt.Sprintf("%v, 今日可卖量可能不准确", err))
		return err
	}
	return nil
}

// runIntradayMonitor 盘中止损监控: 实时价 → 止损检查 → 紧急卖出计划 + 飞书告警
func (s *Scheduler) runIntradayMonitor(date string) error {
	portRepo := store.NewPortfolioRepo(s.db)
	positions, err := portRepo.GetAllPositions()
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
	if s.cfg.Risk.TrailingStopPct > 0 {
		sl.SetTrailingStop(s.cfg.Risk.TrailingStopPct)
	}
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
			HighPrice:    p.HighPrice,
		}
		// 盘中刷新持仓期间最高价 (移动止盈基准)
		if price > p.HighPrice {
			if err := portRepo.UpdateHighPrice(p.TsCode, price); err != nil {
				logger.L().Warnw("更新持仓最高价失败", "ts_code", p.TsCode, "err", err)
			}
		}
		triggered, reason := sl.CheckSingle(pos, price)
		if !triggered {
			continue
		}
		// 数量以可卖量为准并钳制到总持仓, 防止计划卖出量超过实际持仓导致拒单
		qty := p.AvailableQty
		if qty <= 0 {
			qty = p.TotalQty // T+1 不可卖也先生成计划提示
		}
		if qty > p.TotalQty {
			qty = p.TotalQty
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
		msg := fmt.Sprintf("%s 现价 %.2f 触发: %s\n已生成紧急卖出计划(%d股), 请尽快确认", p.TsCode, price, reason, qty)
		// 跌停时止损单可能无法成交, 必须显式提示 (连续跌停场景用户容易无感知)
		if limit, err := store.NewLimitRepo(s.db).GetByCodeAndDate(p.TsCode, date); err == nil && limit != nil &&
			limit.DownLimit > 0 && price <= limit.DownLimit {
			msg += fmt.Sprintf("\n⚠️ 现价已触及跌停价 %.2f, 卖出单可能无法成交, 请立即人工处理!", limit.DownLimit)
		}
		s.alert("🚨 惊蛰盘中止损告警", msg)
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
	if err := store.RunRetention(s.db, store.RetentionPolicy{
		BarYears:     rc.BarYears,
		NewsDays:     rc.NewsDays,
		PlanDays:     rc.PlanDays,
		BacktestRuns: rc.BacktestRuns,
		LogDays:      rc.LogDays,
		ReportFiles:  rc.ReportFiles,
		LogDir:       logDir,
		ReportDir:    "reports",
	}, fullClean); err != nil {
		logger.L().Warnw("数据保留清理失败", "err", err)
	}

	// 清理不在活跃股票池中的陈旧数据 (选股结果+持仓+watchlist 之外的股票)
	if fullClean {
		keepCodes := store.GetActiveStockCodes(s.db, s.cfg.Dataloader.Watchlist, s.cfg.UniverseCodes())
		if err := store.CleanStaleStocks(s.db, keepCodes); err != nil {
			logger.L().Warnw("陈旧股票数据清理失败", "err", err)
		}
	}

	return nil
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
