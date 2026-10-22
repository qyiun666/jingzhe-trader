// Package scheduler tick 循环 · 任务编排 · 补跑 · 冷却 · 优雅关机（§5.2 时间线）。
//
// 自研 tick 不引 cron（§9.3）：到点触发 + 当日未成功补跑 + 失败冷却 30 分钟
// 只需 parseClock + TraceRepo().HasSucceeded + TraceRepo().LastAt 三个判断。
//
// 原则：
//   - panic 只允许在 Scheduler.runJob 中 recover（§11.1）；
//   - 补跑按 run_trace 轨迹判定（重启后自动补跑当日未做成任务，验收 §10.5-9）；
//   - 失败冷却 30 分钟内不重复触发（验收 §10.5-10）；
//   - 时间注入（fake clock）可加速跑完整交易日（验收 §10.5-8）。
package scheduler

import (
	"context"
	"errors"
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

// ErrJobRunning 任务已在执行（手工触发与到点触发撞车）。RunNamed 返回它，
// 调度 Tick 只记日志并跳过本轮，不重复扣 LLM/行情成本。
var ErrJobRunning = errors.New("任务正在运行")

// ErrSkip 本轮无事可做的哨兵（区别于 Degrade）：任务确实在该轮没有任何可交付的工作
// 时用它退出 —— 不写 run_trace 行、不触发完成回调、日志按 info 记。
//
// 为什么不用 Degrade：盘中扫描是每 5 分钟一轮，空仓时一天会留 24 行 partial。
// partial 的语义是"做成了但有已知缺失"，日报要逐条点名，那些行既不是缺失也不是故障，
// 只会把真正的降级淹掉。留痕仍不等于这里可以静默失败 —— ErrSkip 只给"本轮没有工作对象"
// 用（空仓、非交易日），数据缺、依赖挂仍须按 partial / fail 落库。
var ErrSkip = errors.New("本轮无事可做")

// JobSpec 任务规格：到点触发（At）或窗口内间隔触发（EveryMinutes + Windows）。
type JobSpec struct {
	Name         string
	At           []string // HH:MM 触发时刻（可多个，如 data_retry 的 15:25/15:50/16:30）
	EveryMinutes int      // >0 表示窗口内每 N 分钟触发
	Windows      []Window // 间隔触发窗口（EveryMinutes>0 时生效）
	TradeDayOnly bool     // 是否仅交易日运行
	// Timeout 单次执行上限；0 = 不额外限制。外部调用（LLM 600s、SMTP 无上限）
	// 任何一处挂死都会卡住同步的 Tick，从而停掉后续所有任务；按任务类型设上限。
	Timeout time.Duration
	Run     func(ctx context.Context, rc *observability.RunCtx) error
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
	fired        map[string]bool      // job@date@trigger → 已触发
	lastInterval map[string]time.Time // 间隔任务最近触发时刻
	backoffUntil map[string]time.Time // 间隔任务失败后的退避截止时刻（内存态，重启即清）
	runningJobs  map[string]bool      // 正在执行的任务（per-job single-flight）
	lastTick     time.Time            // 最近一轮调度判定时刻（供 /healthz 探活）

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
		backoffUntil: make(map[string]time.Time),
		runningJobs:  make(map[string]bool),
	}
}

// WithClock 注入时钟（fake clock，验收 §10.5-8）。
func (s *Scheduler) WithClock(f func() time.Time) *Scheduler {
	if f != nil {
		s.clock = f
	}
	return s
}

// WithOnDone 注册任务完成回调。
func (s *Scheduler) WithOnDone(f OnJobDone) *Scheduler {
	s.onDone = f
	return s
}

// WithDryRun 演练模式：不执行真实 Run、不写 run_trace（时间线验证用）。
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
	s.mu.Lock()
	s.lastTick = now
	s.mu.Unlock()
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
			status, jerr := s.runJob(ctx, spec, date, trigger, false)
			if status != "" {
				triggered = append(triggered, spec.Name)
				if spec.EveryMinutes > 0 && status == model.TraceFail {
					// 间隔任务这轮失败：退避一个 cooldown，别每 5 分钟撞同一堵墙
					// （到点任务的重试节奏由 shouldSkip 的轨迹冷却承担）。
					s.markIntervalBackoff(spec.Name, now)
				}
				if s.onDone != nil {
					s.onDone(spec, date, status, jerr)
				}
			}
		}
	}
	return triggered
}

// JobCount 已注册任务数（specs 在 New 后不再变更，无需加锁）。
func (s *Scheduler) JobCount() int { return len(s.specs) }

// JobNames 全部任务名（按注册顺序），供外部接口校验入参。
func (s *Scheduler) JobNames() []string {
	out := make([]string, 0, len(s.specs))
	for _, spec := range s.specs {
		out = append(out, spec.Name)
	}
	return out
}

// RunNamed 立即执行指定任务一次（补跑/调试）。走 runJob，
// 因此与到点触发共用同一条 run_trace 落库路径（成功/降级/失败都写当日那一行）。
func (s *Scheduler) RunNamed(ctx context.Context, name, date, trigger string) error {
	for _, spec := range s.specs {
		if spec.Name != name {
			continue
		}
		_, err := s.runJob(ctx, spec, date, trigger, true)
		return err
	}
	return fmt.Errorf("未知任务 %s（可选：%s）", name, strings.Join(s.JobNames(), ", "))
}

// IsRunning 报告主循环是否在跑（Run 退出后为 false）。
func (s *Scheduler) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// LastTickAt 最近一轮调度判定时刻；从未跑过返回零值。
func (s *Scheduler) LastTickAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTick
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
		if s.runningJobs[spec.Name] {
			continue // 手工补跑正在执行：不标记 fired，等它结束后按轨迹补跑语义再判
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
//
// 刻意不走 shouldSkip：那套判定是为到点任务的「当日补跑」设计的 —— 一天做成一次就够。
// 间隔任务每一轮都是独立工作（重新取价、重新判止损），做成一轮不等于当日做完。
// 历史上这里也走 shouldSkip，于是任一轮留下 ok/partial 行就整天不再触发
// （盘中重启后风控失效的根因，2026-10-09 复盘）：09:30 空仓扫描写成 partial，
// 当天晚些时候买入的持仓就再也没被扫过。
//
// 节流由 lastInterval + EveryMinutes 自己承担；上一轮失败额外按 cooldown 退避
// （backoffUntil，只在内存里生效），所以重启后立刻恢复判定，不会拖到明天。
//
// 调用方 dueTriggers 已持 s.mu，本函数不再加锁。
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
	if s.runningJobs[spec.Name] {
		return nil // 上一轮还没跑完，不叠跑；不更新 lastInterval，结束后下一轮立即恢复
	}
	period := time.Duration(spec.EveryMinutes) * time.Minute
	last := s.lastInterval[spec.Name]
	if !last.IsZero() && now.Sub(last) < period {
		return nil
	}
	if until, ok := s.backoffUntil[spec.Name]; ok && now.Before(until) {
		return nil
	}
	s.lastInterval[spec.Name] = now
	return []string{"interval"}
}

// markIntervalBackoff 给刚失败的间隔轮次挂退避（时长 = cooldown）。
// 只有间隔任务用得到：到点任务的重试节奏由 shouldSkip 的轨迹冷却承担。
func (s *Scheduler) markIntervalBackoff(jobName string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backoffUntil[jobName] = at.Add(s.cooldown)
}

// beginJob 尝试占用任务执行权；返回 false 表示同一任务已在运行。
func (s *Scheduler) beginJob(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runningJobs[name] {
		return false
	}
	s.runningJobs[name] = true
	return true
}

// endJob 释放任务执行权。
func (s *Scheduler) endJob(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runningJobs, name)
}

// shouldSkip 判定任务是否应跳过本轮触发（冷却判定，读轨迹表，重启后仍生效）：
// 当日已完成（ok/partial）→ 跳过；最近一次留痕在冷却期内 → 跳过。
func (s *Scheduler) shouldSkip(jobName, date string) bool {
	if s.dryRun {
		return false
	}
	subject := model.TraceJob(jobName)
	ok, err := s.st.TraceRepo().HasSucceeded(context.Background(), subject, date)
	if err == nil && ok {
		return true
	}
	last, err := s.st.TraceRepo().LastAt(context.Background(), subject, date)
	if err != nil || last == "" {
		return false
	}
	t, perr := time.Parse(time.RFC3339, last)
	if perr != nil {
		return false
	}
	return time.Since(t) < s.cooldown
}

// runJob 执行单个任务：跑完写一行轨迹（ok / partial / fail），panic 在此 recover
// （全项目唯一允许 recover 的位置之一，§11.1）。
//
// 不写"开始"行：进程中途死掉就是没有行，而缺行正是自检 [D1] 要抓的信号。
// 不记尝试次数：重试策略只有 cooldown 一个配置项，从来没有次数上限判定。
func (s *Scheduler) runJob(ctx context.Context, spec JobSpec, date, trigger string, manual bool) (string, error) {
	if s.dryRun {
		observability.S().Infow("[dry-run] 任务触发", "job", spec.Name, "date", date, "trigger", trigger)
		return model.TraceOK, nil
	}
	// per-job single-flight：到点触发与手工补跑撞车时只允许一个实例执行。
	// 手工调用返回 ErrJobRunning 让调用方知道没跑；调度 Tick 静默跳过本轮。
	if !s.beginJob(spec.Name) {
		observability.S().Infow("任务已在运行，跳过重复触发",
			"job", spec.Name, "date", date, "trigger", trigger, "manual", manual)
		if manual {
			return "", ErrJobRunning
		}
		return "", nil
	}
	defer s.endJob(spec.Name)

	// 任务级 deadline：外部调用（LLM 单次 600s、SMTP 无上限）任一处挂死都会
	// 卡住同步的 Tick，后面所有任务（含盘中止损扫描）跟着停。超时按 fail 落轨迹，
	// 由 onDone 触发 JOB_FAILED 告警，不静默。
	jobCtx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		jobCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	rc := observability.NewRunCtx(jobCtx, spec.Name, date)
	var jobErr error
	func() {
		defer func() {
			if p := recover(); p != nil {
				jobErr = fmt.Errorf("任务 %s panic: %v", spec.Name, p)
				observability.S().Errorw("任务 panic 已隔离", "job", spec.Name, "panic", fmt.Sprint(p))
			}
		}()
		jobErr = spec.Run(jobCtx, rc)
	}()
	if errors.Is(jobErr, context.DeadlineExceeded) && spec.Timeout > 0 {
		jobErr = fmt.Errorf("任务超时（上限 %s）: %w", spec.Timeout, jobErr)
	}

	// 本轮无事可做（哨兵 ErrSkip）：不写轨迹、不报完成，日志按 info 留一句。
	// 轨迹按 trade_date+subject 幂等覆盖，写了就会把上一轮真正的结果盖掉。
	if errors.Is(jobErr, ErrSkip) {
		observability.S().Infow("任务本轮跳过", "job", spec.Name, "date", date,
			"trigger", trigger, "reason", strings.TrimPrefix(jobErr.Error(), ErrSkip.Error()+": "))
		return "", nil
	}

	outcome, detail := model.TraceOK, ""
	switch {
	case jobErr != nil:
		outcome, detail = model.TraceFail, jobErr.Error()
	default:
		// 产出物契约断言：声明了却没交齐的，折成一句降级说明（[D1]）
		rc.Assert()
		if deg := rc.Degradations(); len(deg) > 0 {
			outcome = model.TracePartial
			parts := make([]string, 0, len(deg))
			for _, d := range deg {
				parts = append(parts, d.Code+": "+d.Reason)
			}
			detail = "降级 " + strings.Join(parts, "；")
		}
	}

	werr := s.st.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: date, Subject: model.TraceJob(spec.Name),
		Outcome: outcome, Detail: detail, At: s.clock().UTC().Format(time.RFC3339),
	})
	if werr != nil {
		observability.S().Errorw("写任务轨迹失败", "job", spec.Name, "err", werr.Error())
	}
	observability.S().Infow("任务完成", "job", spec.Name, "date", date,
		"outcome", outcome, "detail", detail, "trigger", trigger)
	return outcome, jobErr
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
