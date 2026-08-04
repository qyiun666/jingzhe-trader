package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime/debug"
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

	// 日历死锁修复: 检查今天是否在交易日历中
	// 如果日历中没有今天的记录(日历数据过期), 先同步日历再判断
	hasCal, err := s.calRepo.HasDate(today)
	if err != nil {
		logger.L().Warnw("调度器: 查询日历失败", "err", err)
		return
	}
	if !hasCal {
		logger.L().Warnw("调度器: 今日不在交易日历中, 同步日历...", "date", today)
		if err := s.svc.SyncCalendar(); err != nil {
			logger.L().Errorw("调度器: 同步交易日历失败", "err", err)
			return
		}
		logger.L().Info("调度器: 交易日历同步完成, 重新判断交易日")
	}

	isTradeDay, err := s.calRepo.IsTradeDay(today)
	if err != nil {
		logger.L().Warnw("调度器: 判断交易日失败", "err", err)
		return
	}

	if isTradeDay {
		s.maybeRunDaily(JobDataUpdate, s.cfg.Scheduler.DataUpdateTime, now, today, s.runDataUpdate)
		s.maybeRunDaily(JobSignal, s.cfg.Scheduler.SignalTime, now, today, s.runSignal)
		if s.cfg.Broker.Type == "qmt" {
			s.maybeRunDaily(JobReconcile, reconcileTime(s.cfg.Scheduler.SignalTime), now, today, s.runReconcile)
		}
		s.maybeRunDaily(JobReport, s.cfg.Scheduler.ReportTime, now, today, s.runReport)
		s.maybeRunIntraday(now, today)
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

	go func() {
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

// alert 飞书告警 (降级: 发送失败只打日志)
func (s *Scheduler) alert(title, text string) {
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
	if len(plans) > 0 {
		s.alert("📋 惊蛰交易计划", fmt.Sprintf("%s 生成 %d 条交易计划, 请通过 /api/plan 查看并确认", date, len(plans)))
	}
	return nil
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
