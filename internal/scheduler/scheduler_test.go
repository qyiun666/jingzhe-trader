package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// TestLivenessAccessors 探活访问器的初始契约：/healthz 完全依赖这三个值判断调度是否停摆。
func TestLivenessAccessors(t *testing.T) {
	s := New(nil, nil, []JobSpec{{Name: "a"}, {Name: "b"}})

	if s.IsRunning() {
		t.Fatal("未 Run 之前 IsRunning 应为 false")
	}
	if !s.LastTickAt().IsZero() {
		t.Fatal("未 Tick 之前 LastTickAt 应为零值（/healthz 据此判停摆）")
	}
	if got := s.JobCount(); got != 2 {
		t.Fatalf("JobCount 应为 2，实际 %d", got)
	}
}

// TestTickRecordsLastTick Tick 必须推进 lastTick，否则探活会永远报停摆。
func TestTickRecordsLastTick(t *testing.T) {
	s := New(nil, func(string) bool { return true }, nil)
	now := time.Date(2026, 9, 3, 15, 30, 0, 0, time.UTC)

	s.Tick(context.Background(), now)

	got := s.LastTickAt()
	if got.IsZero() {
		t.Fatal("Tick 后 lastTick 未记录")
	}
	if want := "20260903-2330"; got.Format("20060102-1504") != want {
		t.Fatalf("lastTick 应规整到 Asia/Shanghai（15:30Z 应为 %s），实际 %s",
			want, got.Format("20060102-1504"))
	}
}

// TestRunFlipsRunning Run 期间 IsRunning 为真，退出后必须回落 false。
func TestRunFlipsRunning(t *testing.T) {
	s := New(nil, func(string) bool { return true }, nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	if !waitFor(func() bool { return s.IsRunning() }) {
		t.Fatal("Run 启动后 IsRunning 未变为 true")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run 退出应返回 nil，实际 %v", err)
	}
	if s.IsRunning() {
		t.Fatal("Run 退出后 IsRunning 仍为 true")
	}
}

// waitFor 轮询直到 cond 为真或超时。
func waitFor(cond func() bool) bool {
	for i := 0; i < 200; i++ {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// TestScheduleKeysDriveTimeline 触发时刻必须由 scheduler.* 配置键决定，且每个键都真被某个任务读取。
// 键名写错一个字母既不报错也不失败，只会让那个任务永远不触发 —— 这张网兜住它。
// 期望值取自 config.KeySpec 默认值（默认值单点定义），不在测试里再抄一遍字面量。
func TestScheduleKeysDriveTimeline(t *testing.T) {
	// 任务名 → 决定其时刻的配置键。盘中扫描靠窗口+间隔驱动，不占配置键。
	jobKeys := map[string][]string{
		"morning_plan":     {"scheduler.morning"},
		"evening_pipeline": {"scheduler.pipeline"},
		"mail_pending":     {"scheduler.mail_pending"},
		"daily_report":     {"scheduler.report"},
	}
	var want []string
	for job, keys := range jobKeys {
		for _, key := range keys {
			spec, ok := config.FindSpec(key)
			if !ok {
				t.Fatalf("配置键 %s 不在 KeySpec 目录里", key)
			}
			for _, hm := range strings.Split(spec.Default, ",") {
				want = append(want, strings.TrimSpace(hm)+" "+job)
			}
		}
	}

	// 默认值由 config.Load 从 KeySpec 物化，这里用临时库走同一条路径；dry-run 不执行任何真实任务体。
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	t.Setenv("JZ_SERVER_API_TOKEN", "dummy")
	st, err := store.Open(t.TempDir() + "/sched.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	s := New(nil, func(string) bool { return true }, BuildJobs(Deps{Config: cfg})).WithDryRun(true)
	// 刻意不暴露配置键的任务：盘中扫描（窗口 + 间隔驱动）。
	keyless := map[string]bool{"intraday_scan": true}
	got := map[string]bool{}
	for _, line := range s.SimulateDay("20260903") {
		_, name, _ := strings.Cut(line, " ")
		if keyless[name] {
			continue
		}
		got[line] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("按 KeySpec 默认值应触发 %q，实际时间线里没有", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("时间线触发数期望 %d 实际 %d（多出的行说明有任务仍用硬编码时刻）", len(want), len(got))
	}
}

// TestRunJobPersistsArtifacts 回归：run_trace.detail 包含 artifacts 与"缺了什么"都必须落库。
//
// 曾经 RunCtx 只导出 Degradations()，产出物计数没有任何写出方，
// trace.detail 列恒为空串——"每一步都留痕"只剩一句注释。
func TestRunJobPersistsArtifacts(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/art.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()

	s := New(st, func(string) bool { return true }, []JobSpec{{
		Name: "probe",
		Run: func(_ context.Context, rc *observability.RunCtx) error {
			rc.Declare("rows", "candidates", 0)
			rc.Actual("candidates", 7)
			rc.Declare("rows", "pending_tickets", -1) // 故意不产出：应记为缺失并转 degraded
			return nil
		},
	}})
	if err := s.RunNamed(ctx, "probe", "20260901", "manual"); err != nil {
		t.Fatalf("RunNamed 失败: %v", err)
	}

	runs, err := st.TraceRepo().List(ctx, "20260901")
	if err != nil || len(runs) == 0 {
		t.Fatalf("读取 run_trace 失败: runs=%d err=%v", len(runs), err)
	}
	last := runs[len(runs)-1]
	if !strings.Contains(last.Detail, "pending_tickets") {
		t.Errorf("缺产出物应写明缺了哪个，实际 degradations=%q", last.Detail)
	}
	if last.Outcome != string(model.TracePartial) {
		t.Errorf("状态应为 degraded，实际 %s", last.Outcome)
	}
}

// TestRunJobFailureNotMaskedAsDegraded 失败任务不得被降级态覆盖（日报要把失败单独列一列）。
func TestRunJobFailureNotMaskedAsDegraded(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/fail.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()

	s := New(st, func(string) bool { return true }, []JobSpec{{
		Name: "boom",
		Run: func(_ context.Context, rc *observability.RunCtx) error {
			rc.Degrade("SKIPPED", "先降级")
			return errors.New("再失败")
		},
	}})
	_ = s.RunNamed(ctx, "boom", "20260901", "manual")

	traces, _ := st.TraceRepo().List(ctx, "20260901")
	if len(traces) == 0 {
		t.Fatal("无 run_trace 记录")
	}
	var last model.RunTrace
	for _, tr := range traces {
		if tr.Subject == "job:boom" {
			last = tr
		}
	}
	if got := last.Outcome; got != string(model.TraceFail) {
		t.Errorf("有错误时应记 failed，实际 %s", got)
	}
}
