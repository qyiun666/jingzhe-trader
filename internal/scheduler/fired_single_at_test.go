package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// 这组测试回答组长 20261118 的更正问题：
// 「调度器本应自动补跑，今天 16:31 fail 后 2h41m 为什么没有任何第二次执行？」
//
// 答案不在 shouldSkip，而在 dueTriggers 尾部的 fired 预标记（scheduler.go 的
// `s.fired[key] = true` 及其前面三个 continue）：
//
//	key = name@date@hm ; s.fired[key] = true 在**执行之前**就打，且失败不回滚、
//	内存 map 当天不清。所以一个 At 只有一个时刻的任务（evening_pipeline 正是），
//	只要真跑过一次（无论成败）当天就不会再有第二次 —— 下面 shouldSkip 那条
//	「冷却到期自动重试」分支只在 fired[key]==false 时可达，而执行必然已把它置真。
//	唯一的例外是首触那一刻正被冷却/在跑挡住：那两个 continue 不标记 fired，
//	于是同一时刻当天可以延后放行（典型：手工 run task 先写了一行 fail，紧跟着到点）。
//	注释里的"下一窗口重试"只有两种落地形态：① 同一 spec 配多个 At 时刻；② 进程重启
//	（fired 是内存态，重启即清空，启动那一轮立刻按轨迹做补跑判定）。
//
// 这四条测试把这几种形态分别钉死，避免下一个人再按注释推断行为。
//
// 结论（组长 20261118 架构红线后的口径，11-19 定档）：形态① 就是本问题的全部答案 ——
// scheduler.pipeline 配成 "16:30,17:10,18:10,19:10"（2h40m 档，按 20261118 实测故障时长
// 2h05m 反推）即得「当日多次延迟重跑」，jobs.go 的 d.at 天然按逗号切多个时刻，
// **不需要新任务、不需要新配置键**。
// 后几枪的准入判定已在 shouldSkip（HasSucceeded + LastAt 冷却）写死：当日已成 → 跳过，
// 所以首跑成功时绝不会天天重跑整链（不重复烧 LLM 评审、不重复写指令单）；
// 首跑失败时距冷却（30 分钟）已过，直接补跑一次。两个方向各由一条测试钉住。

func atJob(name string, ats []string, runs *int, spec func() error) JobSpec {
	return JobSpec{
		Name: name, At: ats, TradeDayOnly: true,
		Run: func(context.Context, *observability.RunCtx) error {
			*runs++
			if spec != nil {
				return spec()
			}
			return errors.New("① 行情同步: tushare daily 失败 code=50101(transient)")
		},
	}
}

func traceRowsFor(t *testing.T, st *store.Store, name string) []model.RunTrace {
	t.Helper()
	rows, err := st.TraceRepo().List(context.Background(), probeDate)
	if err != nil {
		t.Fatalf("读 run_trace 失败: %v", err)
	}
	var out []model.RunTrace
	for _, r := range rows {
		if r.Subject == model.TraceJob(name) {
			out = append(out, r)
		}
	}
	return out
}

// TestSingleAtJobDoesNotRetrySameDay 复刻 20261118 现场：单 At 时刻、失败、进程不重启。
//
// 这是"为什么今天没有自动补跑"的直接答案 —— 跑一次之后，16:40 到 19:10 的每一个 tick
// 都在 dueTriggers 的 fired 判定处被挡掉，连 shouldSkip 都不会被调用。
func TestSingleAtJobDoesNotRetrySameDay(t *testing.T) {
	var runs int
	s, st := newProbe(t, atJob(JobEveningPipeline, []string{"16:30"}, &runs, nil))

	for _, hm := range []string{"16:30", "16:40", "17:00", "17:30", "18:00", "19:00", "19:10"} {
		s.Tick(context.Background(), clockAt(hm))
	}

	if runs != 1 {
		t.Errorf("单 At 时刻任务当天执行 %d 次，期望 1（fired 预标记挡住了同刻重试）", runs)
	}
	rows := traceRowsFor(t, st, JobEveningPipeline)
	if len(rows) != 1 {
		t.Fatalf("run_trace 应有 1 行，实际 %d 行", len(rows))
	}
	if rows[0].Outcome != model.TraceFail {
		t.Errorf("outcome = %s，期望 fail", rows[0].Outcome)
	}
}

// TestRestartClearsFiredAndReruns 注释里的"补跑语义"真正成立的场景：进程重启。
//
// 库里已有当日 fail 行且已过 30 分钟冷却，换一个**新调度器实例**（内存 fired 为空）
// 做一轮判定 → 应当补跑一次。这就是 11-14 容器重启后当天任务能追回来的机制，
// 也是今天没生效的原因：PID 137731 从 11-16 18:38 起没重启过。
func TestRestartClearsFiredAndReruns(t *testing.T) {
	_, st := newProbe(t) // 只借一个库
	aged := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	if err := st.TraceRepo().Write(context.Background(), model.RunTrace{
		TradeDate: probeDate, Subject: model.TraceJob(JobEveningPipeline),
		Outcome: model.TraceFail, Detail: "首跑失败（老化 2 小时以越过冷却）", At: aged,
	}); err != nil {
		t.Fatalf("预置 fail 行失败: %v", err)
	}

	var runs int
	// 关键在"新实例"：fired 是内存态（Scheduler.fired 字段，New 时重建），
	// 重启即清空 —— 这就是进程重启后当日未做成任务能追回来的机制。
	s := New(st, func(string) bool { return true },
		[]JobSpec{atJob(JobEveningPipeline, []string{"16:30"}, &runs, func() error { return nil })})
	s.Tick(context.Background(), clockAt("19:12"))
	if runs != 1 {
		t.Fatalf("重启后应补跑一次，实际 %d 次", runs)
	}

	// 同一实例再走一轮：fired 已标记，不会再补跑第二次（与 TestSingleAtJobDoesNotRetrySameDay 同因）。
	s.Tick(context.Background(), clockAt("19:20"))
	if runs != 1 {
		t.Errorf("补跑后又触发了第 %d 次，期望仍为 1", runs)
	}
}

// TestMultiAtRerunOverwritesOneTraceRow 形态① 确实会自动重跑（把 scheduler.pipeline
// 配成多个逗号分隔的时刻，零新增机制），但代价在这里钉成事实且已被组长接受：
// 两次执行**共用同一行**
// job:evening_pipeline，第二次把首跑的 fail 整行盖掉
// （repo_trace.go 的 ON CONFLICT DO UPDATE 覆盖 outcome/detail/at 三列）。
//
// 这条代价可以接受，是因为首跑失败并非在库里整条消失 —— 同一根因还有一行
// alert:JOB_FAILED:evening_pipeline，由 cmd/jingzhe/main.go 装配的 OnJobDone 在失败当时落，
// 成功那一轮既不报警也不写这个 subject，所以不会覆盖首跑的原文与时刻。
// 20261118 实测：job 行（id 3833）
// 的 at=10:47:31Z 已被当日后续执行覆盖，alert 行（id 3834）的 at=08:31:01Z 仍带
// 首跑 16:31 那句原文（dial tcp 10.255.255.2:8806: connection refused）。
// 这个"后续成功不改写告警行"正是四时刻方案的承重假设，由
// job_failed_alert_test.go 单独钉死（组长 20261119 验收硬项①）。
// 本测试的探针没接 OnJobDone（newProbe 只装 New 的三参），所以库内只剩 job 行 ——
// 断言描述的是 job 行的覆盖语义，不代表生产无任何痕迹。
func TestMultiAtRerunOverwritesOneTraceRow(t *testing.T) {
	var runs int
	var failFirst = func() error {
		runs++
		if runs == 1 {
			return errors.New("① 行情同步: tushare daily 失败 code=50101(transient) msg=dial tcp")
		}
		return nil // 第二次上游恢复
	}
	s, st := newProbe(t, JobSpec{
		Name: JobEveningPipeline, At: []string{"16:30", "17:10"}, TradeDayOnly: true,
		Run: func(context.Context, *observability.RunCtx) error { return failFirst() },
	})
	// 把冷却归零，单独观察 fired 的作用：生产中 16:32→17:10 已过 38 分钟，本就超出
	// 30 分钟冷却，所以这不是人为放宽条件，而是等价于当时的真实节奏。
	s.cooldown = 0

	s.Tick(context.Background(), clockAt("16:30"))
	s.Tick(context.Background(), clockAt("17:10"))

	if runs != 2 {
		t.Fatalf("多时刻应执行 2 次，实际 %d 次", runs)
	}
	rows := traceRowsFor(t, st, JobEveningPipeline)
	if len(rows) != 1 {
		t.Fatalf("多时刻共用 subject，应只有 1 行，实际 %d 行", len(rows))
	}
	if rows[0].Outcome != model.TraceOK {
		t.Errorf("期望首跑 fail 行被重跑结果盖成 ok，实际 outcome=%q detail=%q", rows[0].Outcome, rows[0].Detail)
	}
	// P2 之后 ok 行的 detail 不再为空（末尾带「触发=scheduler@17:10」观测标注），
	// 所以"被盖掉"要看正文：首跑那句失败原因必须已经不在行里。
	if strings.Contains(rows[0].Detail, "tushare daily 失败") {
		t.Errorf("重跑 ok 后仍残留首跑失败原因，覆盖语义不成立：%q", rows[0].Detail)
	}
	if !strings.Contains(rows[0].Detail, "触发=scheduler@17:10") {
		t.Errorf("ok 行应带上本次执行的触发标注，实际 %q", rows[0].Detail)
	}
	// 本探针里 job 行是唯一的当日留痕，重跑成功后 fail 一行不剩（代价写死在这里，
	// 生产侧的补充痕迹见上方注释：alert:JOB_FAILED 行不受覆盖）。
	if all, err := st.TraceRepo().List(context.Background(), probeDate); err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	} else {
		for _, r := range all {
			if r.Outcome == model.TraceFail {
				t.Errorf("重跑后仍留有 fail 行 %s，与「覆盖」语义不符", r.Subject)
			}
		}
	}
}

// TestMultiAtSecondSlotSkippedWhenAlreadySucceeded 是形态① 可用的前提证明：
// 首跑成功时，后面三个时刻必须都被 shouldSkip（HasSucceeded 判据）挡掉 ——
// 否则"多配几个时刻"就变成了每天把整条收盘流水线跑四遍，
// 重复烧 LLM 评审额度、重复改指令单，红线让新增的机制会引入这个风险，而既有语义不会。
//
// 时刻取生产 20261119 定档的那一组（16:30,17:10,18:10,19:10），四枪逐枪验。
// 冷却时长在这里不影响结论（HasSucceeded 在时间判定之前就短路），故不人为归零。
func TestMultiAtSecondSlotSkippedWhenAlreadySucceeded(t *testing.T) {
	var runs int
	s, st := newProbe(t, JobSpec{
		Name: JobEveningPipeline, At: []string{"16:30", "17:10", "18:10", "19:10"}, TradeDayOnly: true,
		Run: func(context.Context, *observability.RunCtx) error { runs++; return nil },
	})

	for _, hm := range []string{"16:30", "17:00", "17:10", "17:15", "18:10", "19:10", "19:40"} {
		s.Tick(context.Background(), clockAt(hm))
	}

	if runs != 1 {
		t.Errorf("首跑已 ok，多时刻仍执行 %d 次，期望 1（shouldSkip 未生效）", runs)
	}
	rows := traceRowsFor(t, st, JobEveningPipeline)
	if len(rows) != 1 || rows[0].Outcome != model.TraceOK {
		t.Fatalf("期望恰好 1 行 ok，实际 %+v", rows)
	}
}
