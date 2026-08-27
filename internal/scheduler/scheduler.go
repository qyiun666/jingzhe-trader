package scheduler

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/api"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// settleT1Time T+1 持仓结转时间 (开盘前, 盘中监控启动前)
const settleT1Time = "09:25"

// tickInterval 调度检查周期: 每30秒检查一次是否有到点任务
const tickInterval = 30 * time.Second

// jobRetryCooldown 任务失败后的冷却期: 失败任务30分钟内不重试, 避免每30秒重试刷爆告警表
const jobRetryCooldown = 30 * time.Minute

// Scheduler 内置调度器
// 交易日自动执行: 盘前总结 → 数据更新 → 选股 → EOD信号 → 当天总结/日报 → 数据清理; 盘中定时止损监控
// 标准库 time.Ticker 实现, 所有任务经 runJob wrapper (recover + job_run 记录 + 启动补跑)
type Scheduler struct {
	cfg      *config.Config
	db       *sqlx.DB
	svc      *api.Service
	mailer   *notify.MailNotifier
	quoteSrc quote.Source

	jobRepo  *store.JobRepo
	planRepo *store.PlanRepo
	calRepo  *store.CalendarRepo

	running      sync.Map       // job_name -> bool, 防止同名任务重叠执行
	goalMu       sync.Mutex     // 串行化 checkGoalMode: data_update 重试与 signal 补记可能同日并发评估 (读-改-写非原子, 防重复告警)
	lastIntraday time.Time      // 上一轮盘中监控时间
	jobWg        sync.WaitGroup // 等待所有 job goroutine 完成 (优雅关闭用)
	mailWarnMu   sync.Mutex     // 串行化"邮件未配置"每日告警去重
	mailWarnDate string         // 最近一次该告警的日期 (当日只提醒一次)
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
		mailer:   notify.NewMailNotifier(cfg.Mail.Enabled, cfg.Mail.From, cfg.Mail.Password),
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
		s.maybeRunDaily(store.JobRetention, s.cfg.Retention.CleanupTime, now, today, func(date string) error {
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
		s.maybeRunDaily(store.JobPremarket, s.cfg.Scheduler.PremarketTime, now, today, s.runPremarket)
		s.maybeRunDaily(store.JobSettleT1, settleT1Time, now, today, s.runSettleT1)
		s.maybeRunDataUpdateWithRetry(now, today)
		s.maybeRunDaily(store.JobScreener, s.cfg.Scheduler.ScreenerTime, now, today, s.runScreener)
		s.maybeRunDaily(store.JobSignal, s.cfg.Scheduler.SignalTime, now, today, s.runSignalWithFreshnessCheck)
		if s.cfg.Broker.Type == "qmt" {
			s.maybeRunDaily(store.JobReconcile, reconcileTime(s.cfg.Scheduler.SignalTime), now, today, s.runReconcile)
		}
		s.maybeRunDaily(store.JobReport, s.cfg.Scheduler.ReportTime, now, today, func(date string) error {
			// 日报依赖信号完成 (18:00 同时到点, 信号含 LLM 辩论可能耗时数分钟):
			// 信号运行中则本轮跳过, 下一 tick 再检查; 信号已结束(成功或失败)则正常生成日报
			if _, running := s.running.Load(store.JobSignal); running {
				logger.L().Infow("调度器: 信号任务运行中, 日报等待下一轮", "date", date)
				return nil
			}
			return s.runReport(date)
		})
		s.maybeRunIntraday(now, today)
	} else {
		logger.L().Infow("调度器: 今天是节假日(数据库确认), 跳过交易任务", "date", today)
	}
	// 数据清理每日执行 (非交易日仅做文件清理与WAL瘦身)
	s.maybeRunDaily(store.JobRetention, s.cfg.Retention.CleanupTime, now, today, func(date string) error {
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
	// 失败冷却: 上次尝试失败且在冷却期内不重试, 避免无退避重试刷爆告警
	if last, _ := s.jobRepo.LastAttemptStartedAt(name, today); !last.IsZero() && now.Sub(last) < jobRetryCooldown {
		return
	}
	s.runJob(name, today, fn)
}

// maybeRunDataUpdateWithRetry 数据更新支持多重试时间
// 首次在 data_update_time 执行, 失败后在 signal_time 和 signal_time+30min 自动重试
// 重试间隔通过 job_run 表的上次尝试时间判断, 避免同一窗口内重复执行
func (s *Scheduler) maybeRunDataUpdateWithRetry(now time.Time, today string) {
	if done, err := s.jobRepo.HasSucceeded(store.JobDataUpdate, today); err != nil || done {
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
		lastAttempt, _ := s.jobRepo.LastAttemptStartedAt(store.JobDataUpdate, today)
		if lastAttempt.IsZero() || lastAttempt.Before(scheduled) {
			s.runJob(store.JobDataUpdate, today, s.runDataUpdate)
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
	s.runJob(store.JobIntraday, today, s.runIntradayMonitor)
}

// runJob 统一任务执行 wrapper: 互斥防重叠 + recover隔离 + job_run 记录 + 失败告警
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

// alert 告警落库 (Agent 可通过 /api/agent/alerts 读取)
// 设计: 任务失败/数据更新等过程告警不单独发邮件打扰, 统一汇总进 18:00 日报邮件;
// 需用户操作的通知 (止损触发/盘前总结/日报/买卖指令) 由各场景显式调用 mailer 发送
func (s *Scheduler) alert(title, text string) {
	// 落库 (始终执行, 存一份供 Agent 读取)
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
}

// warnMailDisabled 邮件通知未完整配置时, 当日仅落一条告警留痕 (此后当日静默跳过)
// 背景: MailNotifier 对未配置状态是静默 no-op, 曾出现任务全绿但一封邮件都没发的无声故障
func (s *Scheduler) warnMailDisabled(scene string) {
	s.mailWarnMu.Lock()
	defer s.mailWarnMu.Unlock()
	today := time.Now().Format("20060102")
	if s.mailWarnDate == today {
		return
	}
	s.mailWarnDate = today
	s.alert("⚠️ 邮件通知未配置", fmt.Sprintf("今日推送已跳过(%s): 需同时满足 enabled=true、from 非空、JZ_MAIL_PASSWORD 已注入", scene))
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
