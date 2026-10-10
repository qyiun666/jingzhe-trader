package scheduler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// 这组测试钉住 2026-10-09 的两处调度语义：
//
//	[P0] 间隔任务（EveryMinutes>0）不吃 shouldSkip 的"当日已做成"判定。
//	       事故形态：09:30 空仓扫描留下 partial 行 → HasSucceeded 认 partial →
//	       全天不再触发 → 当天买入的持仓再也没被扫过，盘中止损整段失效。
//	[P2] 本轮无事可做（ErrSkip）不写轨迹行，也不盖掉上一轮真正的结果。
//
// 到点任务的 partial==已做成 语义必须原样保留（邮件/日报/自检都依赖它），
// 所以两套语义各测一遍：改错了任何一边都能在这里炸出来。

const probeDate = "20260903"

// clockAt 把 "HH:MM" 落成 Asia/Shanghai 的当日时刻（Tick 内部按此时区取日期）。
func clockAt(hm string) time.Time {
	t, err := time.ParseInLocation("20060102 15:04", probeDate+" "+hm, market.Loc)
	if err != nil {
		panic(err)
	}
	return t
}

// everyJob 一个每 5 分钟跑一轮的盘中任务替身，数执行次数。
func everyJob(name string, runs *int, spec func() error) JobSpec {
	return JobSpec{
		Name: name, EveryMinutes: 5,
		Windows: []Window{{Start: "09:30", End: "11:30"}},
		Run: func(context.Context, *observability.RunCtx) error {
			*runs++
			if spec != nil {
				return spec()
			}
			return nil
		},
	}
}

func writeTrace(t *testing.T, st *store.Store, name, outcome, detail string) {
	t.Helper()
	if err := st.TraceRepo().Write(context.Background(), model.RunTrace{
		TradeDate: probeDate, Subject: model.TraceJob(name),
		Outcome: outcome, Detail: detail,
		At: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("预置 run_trace 失败: %v", err)
	}
}

func traceOf(t *testing.T, st *store.Store, name string) (model.RunTrace, bool) {
	t.Helper()
	rows, err := st.TraceRepo().List(context.Background(), probeDate)
	if err != nil {
		t.Fatalf("读 run_trace 失败: %v", err)
	}
	for _, r := range rows {
		if r.Subject == model.TraceJob(name) {
			return r, true
		}
	}
	return model.RunTrace{}, false
}

// newProbe 建一个带临时库、天天都是交易日的调度器。
func newProbe(t *testing.T, specs ...JobSpec) (*Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, func(string) bool { return true }, specs), st
}

// TestIntervalFiresDespiteDoneTrace [P0] 当天已有 ok 或 partial 行，间隔任务仍须每轮触发。
func TestIntervalFiresDespiteDoneTrace(t *testing.T) {
	for _, done := range []string{model.TracePartial, model.TraceOK} {
		t.Run(done, func(t *testing.T) {
			var runs int
			s, st := newProbe(t, everyJob("intraday_probe", &runs, nil))
			// 复刻现场：先给当天留一行"已做成"。
			writeTrace(t, st, "intraday_probe", done, "预置")

			for _, hm := range []string{"09:30", "09:35", "09:40"} {
				s.Tick(context.Background(), clockAt(hm))
			}
			if runs != 3 {
				t.Errorf("当天已有 %s 行时，三轮 5 分钟间隔应各触发一次，实际执行 %d 次"+
					"（把 partial 当完成就是盘中风控失效的根因）", done, runs)
			}
			// 触发过的轮次必须真的留下轨迹，否则自检 [D1] 抓不到后续失败。
			if row, ok := traceOf(t, st, "intraday_probe"); !ok || row.Outcome != model.TraceOK {
				t.Errorf("间隔轮次应落轨迹行，实际 outcome=%q 存在=%v", row.Outcome, ok)
			}
		})
	}
}

// TestPointJobStillSkipsAfterDoneTrace [P0 的另一半] 到点任务维持"当天做成一次"语义。
// 如果哪天有人把 shouldSkip 整个删掉，这个测试会红：邮件会重复发、日报会重复出。
func TestPointJobStillSkipsAfterDoneTrace(t *testing.T) {
	for _, done := range []string{model.TracePartial, model.TraceOK} {
		t.Run(done, func(t *testing.T) {
			var runs int
			s, st := newProbe(t, JobSpec{
				Name: "daily_probe", At: []string{"18:00"},
				Run: func(context.Context, *observability.RunCtx) error { runs++; return nil },
			})
			writeTrace(t, st, "daily_probe", done, "预置")

			s.Tick(context.Background(), clockAt("18:00"))
			if runs != 0 {
				t.Errorf("到点任务当天已有 %s 行时不应再跑，实际跑了 %d 次", done, runs)
			}
		})
	}
}

// TestIntervalFailureBacksOffButRecovers 间隔任务失败一轮：按 cooldown 退避，
// 退避到期后自动恢复（不拖到明天）；退避只走内存，不受轨迹行影响。
func TestIntervalFailureBacksOffButRecovers(t *testing.T) {
	var runs int
	s, _ := newProbe(t, everyJob("flaky", &runs, func() error {
		return fmt.Errorf("取价失败")
	}))
	cooldown := JobStatusCoolDown

	s.Tick(context.Background(), clockAt("09:30")) // 失败 → 挂退避
	s.Tick(context.Background(), clockAt("09:35")) // 退避期内：5 分钟到点也不跑
	s.Tick(context.Background(), clockAt("09:40"))
	if runs != 1 {
		t.Errorf("失败后 %v 内应退避，实际执行 %d 次", cooldown, runs)
	}
	s.Tick(context.Background(), clockAt("10:00").Add(cooldown)) // 退避到期：恢复
	if runs != 2 {
		t.Errorf("退避到期后应重新触发（当天不封顶），实际执行 %d 次", runs)
	}
}

// TestErrSkipWritesNoTrace [P2] 空仓那类"本轮无事可做"不留任何轨迹行。
// 走 Degrade 的话一天会堆 24 行 partial，把真正的降级淹掉。
func TestErrSkipWritesNoTrace(t *testing.T) {
	var doneCalled int
	s, st := newProbe(t, JobSpec{
		Name: "empty_probe", EveryMinutes: 5,
		Windows: []Window{{Start: "09:30", End: "11:30"}},
		Run: func(_ context.Context, rc *observability.RunCtx) error {
			rc.Declare("rows", "positions", -1) // 声明了却注定不产出：若误走 Degrade 就会写 partial
			return fmt.Errorf("%w: 无持仓", ErrSkip)
		},
	})
	s = s.WithOnDone(func(JobSpec, string, string, error) { doneCalled++ })

	triggered := s.Tick(context.Background(), clockAt("09:30"))
	if len(triggered) != 0 {
		t.Errorf("ErrSkip 轮次不该算作已触发，实际 %v", triggered)
	}
	if _, ok := traceOf(t, st, "empty_probe"); ok {
		t.Error("ErrSkip 轮次不应写 run_trace 行")
	}
	if doneCalled != 0 {
		t.Errorf("ErrSkip 轮次不应触发完成回调（会误报 JOB_FAILED），实际 %d 次", doneCalled)
	}
}

// TestIntradayScanNoHoldingsLeavesNoTrace 把 P2 钉在真正的任务体上：空仓那一轮
// （position 表里没有 total_qty>0 的行）不写任何轨迹行。这是 09:30 那行 partial 的出处。
func TestIntradayScanNoHoldingsLeavesNoTrace(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/nolot.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()

	var runs int
	s := New(st, func(string) bool { return true }, []JobSpec{{
		Name: "intraday_scan", EveryMinutes: 5,
		Windows: []Window{{Start: "09:30", End: "11:30"}},
		Run: func(c context.Context, rc *observability.RunCtx) error {
			runs++
			return intradayScan(c, rc, Deps{Store: st})
		},
	}})
	s.Tick(context.Background(), clockAt("09:30"))
	if runs != 1 {
		t.Fatalf("任务体应被执行一次，实际 %d 次", runs)
	}
	if row, ok := traceOf(t, st, "intraday_scan"); ok {
		t.Errorf("空仓轮次不该留轨迹（历史上这里写 partial，直接把当天剩下的盘中扫描全关掉），实际 %s/%q",
			row.Outcome, row.Detail)
	}
}

// TestErrSkipKeepsEarlierOutcome [P2 的另一半] 轨迹按 (日期, subject) 幂等覆盖，
// 跳过轮次若写行就会把上一轮真正的失败结果盖掉 —— 失败必须仍然看得见。
func TestErrSkipKeepsEarlierOutcome(t *testing.T) {
	var flip = false
	s, st := newProbe(t, JobSpec{
		Name: "keepalive", EveryMinutes: 5,
		Windows: []Window{{Start: "09:30", End: "11:30"}},
		Run: func(context.Context, *observability.RunCtx) error {
			if flip {
				return fmt.Errorf("%w: 无持仓", ErrSkip)
			}
			return fmt.Errorf("行情源挂了")
		},
	})

	s.Tick(context.Background(), clockAt("09:30")) // 失败
	row, ok := traceOf(t, st, "keepalive")
	if !ok || row.Outcome != model.TraceFail {
		t.Fatalf("首轮失败应留 fail 行，实际 outcome=%q 存在=%v", row.Outcome, ok)
	}

	flip = true
	s.Tick(context.Background(), clockAt("10:00").Add(JobStatusCoolDown)) // 退避到期后空仓跳过
	row, _ = traceOf(t, st, "keepalive")
	if row.Outcome != model.TraceFail {
		t.Errorf("跳过轮次把上一轮的 fail 盖成了 %q，当天真正的故障就从日报里消失了", row.Outcome)
	}
}
