// Package scheduler tick 循环 · 任务编排 · 补跑 · 冷却 · 优雅关机（§5.2 时间线）。
//
// 自研 tick 不引 cron（§9.3）：到点触发 + 当日未成功补跑 + 失败冷却 30 分钟
// 只需 parseClock + HasSucceeded + LastJobAttemptAt 三个判断。
//
// 原则：
//   - panic 只允许在 Scheduler.runJob 中 recover（§11.1）；
//   - 补跑按 job_run 表判定（重启后自动补跑当日未成功任务，验收 §10.5-9）；
//   - 失败冷却 30 分钟内不重复触发（验收 §10.5-10）；
//   - 时间注入（fake clock）可加速跑完整交易日（验收 §10.5-8）。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// JobStatusCoolDown 失败冷却时长（附录 A）。
const JobStatusCoolDown = 30 * time.Minute

// DefaultTick 默认调度 tick（附录 A）。
const DefaultTick = 30 * time.Second

// Window 间隔触发窗口（含头不含尾，§5.3 盘中监控）。
type Window struct {
	Start string // HH:MM
	End   string // HH:MM
}

// JobSpec 任务规格：到点触发（At）或窗口内间隔触发（EveryMinutes + Windows）。
type JobSpec struct {
	Name         string
	At           []string // HH:MM 触发时刻（可多个，如 data_retry 的 15:25/15:50/16:30）
	EveryMinutes int      // >0 表示窗口内每 N 分钟触发
	Windows      []Window // 间隔触发窗口（EveryMinutes>0 时生效）
	TradeDayOnly bool     // 是否仅交易日运行
	Run          func(ctx context.Context, rc *observability.RunCtx) error
}

// dueToday 判断 now 是否落在间隔窗口内（纯函数）。
func inWindow(now time.Time, w Window) bool {
	hm := now.Format("15:04")
	return hm >= w.Start && hm < w.End
}

// parseClock 解析 "HH:MM" 为当日时刻（Loc 时区）。
func parseClock(date, hm string) (time.Time, error) {
	parts := strings.Split(hm, ":")
	if len(parts) != 2 {
		return time.Time{}, fmt.Errorf("非法时刻 %q（应为 HH:MM）", hm)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return time.Time{}, fmt.Errorf("非法时刻 %q", hm)
	}
	t, err := time.ParseInLocation("20060102", date, market.Loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("解析日期 %s 失败: %w", date, err)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), h, m, 0, 0, market.Loc), nil
}

// OnJobDone 任务完成回调（装配层用于落 JOB_FAILED 告警）。
type OnJobDone func(spec JobSpec, tradeDate, status string, jobErr error)

// Scheduler 调度器。
type Scheduler struct {
	st         *store.Store
	specs      []JobSpec
	clock      func() time.Time
	tick       time.Duration
	cooldown   time.Duration
	isTradeDay func(date string) bool
	onDone     OnJobDone
	dryRun     bool

	mu           sync.Mutex
	fired        map[string]bool // job@date@trigger → 已触发
	lastInterval map[string]time.Time // 间隔任务最近触发时刻

	running bool
}

// New 构造调度器。isTradeDay 由调用方传入（查 trade_cal 表）。
func New(st *store.Store, isTradeDay func(date string) bool, specs []JobSpec) *Scheduler {
	return &Scheduler{
		st:           st,
		specs:        specs,
		clock:        time.Now,
		tick:         DefaultTick,
		cooldown:     JobStatusCoolDown,
		isTradeDay:   isTradeDay,
		fired:        make(map[string]bool),
		lastInterval: make(map[string]time.Time),
	}
}

// WithClock 注入时钟（fake clock，验收 §10.5-8）。
func (s *Scheduler) WithClock(f func() time.Time) *Scheduler {
	if f != nil {
		s.clock = f
	}
	return s
}

// WithTick 注入 tick 间隔（测试加速用）。
func (s *Scheduler) WithTick(d time.Duration) *Scheduler {
	if d > 0 {
		s.tick = d
	}
	return s
}

// WithCooldown 注入冷却时长（测试用；生产默认 30 分钟）。
func (s *Scheduler) WithCooldown(d time.Duration) *Scheduler {
	if d > 0 {
		s.cooldown = d
	}
	return s
}

// WithOnDone 注册任务完成回调。
func (s *Scheduler) WithOnDone(f OnJobDone) *Scheduler {
	s.onDone = f
	return s
}

// WithDryRun 演练模式：不执行真实 Run、不写 job_run（时间线验证用）。
func (s *Scheduler) WithDryRun(b bool) *Scheduler {
	s.dryRun = b
	return s
}

// Run 主循环：每 tick 执行一次调度判定，直到 ctx 取消（优雅关机：等待当前任务完成）。
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("调度器已在运行")
	}
	s.running = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.running = false; s.mu.Unlock() }()

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	// 启动即先做一轮判定（补跑入口：重启后立刻追上已过期时刻）
	if trigs := s.Tick(ctx, s.clock()); len(trigs) > 0 {
		observability.S().Infow("启动补跑判定完成", "triggered", strings.Join(trigs, ","))
	}
	for {
		select {
		case <-ctx.Done():
			observability.S().Infow("调度器优雅关机")
			return nil
		case <-ticker.C:
			s.Tick(ctx, s.clock())
		}
	}
}

// Tick 执行一轮调度判定，返回本轮实际触发（含跳过原因外的真实执行）的任务名列表。
// now 由注入时钟提供；测试可逐次推进 fake clock。
func (s *Scheduler) Tick(ctx context.Context, now time.Time) []string {
	now = now.In(market.Loc)
	date := now.Format("20060102")
	tradeDay := true
	if s.isTradeDay != nil {
		tradeDay = s.isTradeDay(date)
	}
	var triggered []string
	for _, spec := range s.specs {
		if spec.TradeDayOnly && !tradeDay {
			continue
		}
		for _, trigger := range s.dueTriggers(spec, date, now) {
			status, jerr := s.runJob(ctx, spec, date, trigger)
			if status != "" {
				triggered = append(triggered, spec.Name)
			}
			if s.onDone != nil && status != "" {
				s.onDone(spec, date, status, jerr)
			}
		}
	}
	return triggered
}

// dueTriggers 计算某任务当前应触发的 trigger 列表（含冷却/已完成判定）。
func (s *Scheduler) dueTriggers(spec JobSpec, date string, now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if spec.EveryMinutes > 0 {
		return s.dueInterval(spec, date, now)
	}
	var due []string
	for _, hm := range spec.At {
		t, err := parseClock(date, hm)
		if err != nil {
			observability.S().Errorw("任务时刻解析失败", "job", spec.Name, "at", hm, "err", err.Error())
			continue
		}
		if now.Before(t) {
			continue
		}
		key := spec.Name + "@" + date + "@" + hm
		if s.fired[key] {
			continue
		}
		if s.shouldSkip(spec.Name, date) {
			continue // 冷却中：不标记 fired，冷却到期自动重试（补跑语义）
		}
		s.fired[key] = true // 预标记：无论成败，本次窗口只执行一次；失败靠冷却+下一窗口重试
		due = append(due, hm)
	}
	return due
}

// dueInterval 间隔任务的到期判定（盘中监控每 5 分钟）。
func (s *Scheduler) dueInterval(spec JobSpec, date string, now time.Time) []string {
	inWin := false
	for _, w := range spec.Windows {
		if inWindow(now, w) {
			inWin = true
			break
		}
	}
	if !inWin {
		return nil
	}
	last := s.lastInterval[spec.Name]
	if !last.IsZero() && now.Sub(last) < time.Duration(spec.EveryMinutes)*time.Minute {
		return nil
	}
	if s.shouldSkip(spec.Name, date) {
		return nil
	}
	s.lastInterval[spec.Name] = now
	return []string{"interval"}
}

// shouldSkip 判定任务是否应跳过本轮触发（冷却判定，读 job_run 表，重启后仍生效）：
// 当日已完成（success/degraded）→ 跳过；最近一次尝试在冷却期内 → 跳过。
func (s *Scheduler) shouldSkip(jobName, date string) bool {
	if s.dryRun {
		return false
	}
	ok, err := s.st.OpsRepo().HasJobSucceeded(context.Background(), jobName, date)
	if err == nil && ok {
		return true
	}
	last, err := s.st.OpsRepo().LastJobAttemptAt(context.Background(), jobName, date)
	if err != nil || last == "" {
		return false
	}
	t, perr := time.Parse(time.RFC3339, last)
	if perr != nil {
		return false
	}
	return time.Since(t) < s.cooldown
}

// runJob 执行单个任务：job_run 全程落库（running → 终态），
// panic 在此 recover（全项目唯一允许 recover 的位置之一，§11.1）。
func (s *Scheduler) runJob(ctx context.Context, spec JobSpec, date, trigger string) (string, error) {
	if s.dryRun {
		observability.S().Infow("[dry-run] 任务触发", "job", spec.Name, "date", date, "trigger", trigger)
		return string(model.JobSuccess), nil
	}
	attempt := 1
	if n, err := s.st.OpsRepo().JobAttempts(ctx, spec.Name, date); err == nil {
		attempt = n + 1
	}
	started := s.clock().UTC().Format(time.RFC3339)
	startReal := time.Now()
	_ = s.st.OpsRepo().UpsertJobRun(ctx, model.JobRun{
		JobName: spec.Name, TradeDate: date, Attempt: attempt,
		Status: string(model.JobRunning), StartedAt: started,
	})

	rc := observability.NewRunCtx(ctx, spec.Name, date)
	var jobErr error
	status := string(model.JobSuccess)
	func() {
		defer func() {
			if p := recover(); p != nil {
				jobErr = fmt.Errorf("任务 %s panic: %v", spec.Name, p)
				status = string(model.JobFailed)
				observability.S().Errorw("任务 panic 已隔离", "job", spec.Name, "panic", fmt.Sprint(p))
			}
		}()
		if rerr := spec.Run(ctx, rc); rerr != nil {
			jobErr = rerr
			status = string(model.JobFailed)
		}
	}()
	if jobErr == nil {
		misses := rc.Assert() // 产出物契约断言：缺失自动转 degraded（D1）
		if rc.Degraded() {
			status = string(model.JobDegraded)
		}
		_ = misses
	}
	duration := time.Since(startReal).Milliseconds()
	finished := s.clock().UTC().Format(time.RFC3339)
	deg, _ := json.Marshal(rc.Degradations())
	if err := s.st.OpsRepo().UpsertJobRun(ctx, model.JobRun{
		JobName: spec.Name, TradeDate: date, Attempt: attempt,
		Status: status, DurationMs: duration,
		Error: errStr(jobErr), Degradations: string(deg),
		StartedAt: started, FinishedAt: finished,
	}); err != nil {
		observability.S().Errorw("回写任务终态失败", "job", spec.Name, "err", err.Error())
	}
	observability.S().Infow("任务完成", "job", spec.Name, "date", date, "attempt", attempt,
		"status", status, "duration_ms", duration, "err", errStr(jobErr))
	return status, jobErr
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// SimulateDay 用注入时钟顺序推演一个完整交易日（06:50→21:00，步长 1 分钟），
// 返回按触发顺序的 "HH:MM job" 列表（验收 §10.5-8；须配合 WithDryRun 使用）。
func (s *Scheduler) SimulateDay(date string) []string {
	t0, err := time.ParseInLocation("20060102", date, market.Loc)
	if err != nil {
		return nil
	}
	var out []string
	start := time.Date(t0.Year(), t0.Month(), t0.Day(), 6, 50, 0, 0, market.Loc)
	for cur := start; cur.Format("15:04") <= "21:00"; cur = cur.Add(time.Minute) {
		for _, name := range s.Tick(context.Background(), cur) {
			out = append(out, fmt.Sprintf("%s %s", cur.Format("15:04"), name))
		}
	}
	return out
}
