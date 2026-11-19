package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// alertRow 按 alert:<code> 口径取当日一行（不能复用 traceOf：那个只匹配 job: 前缀）。
func alertRow(t *testing.T, st *store.Store, code string) (model.RunTrace, bool) {
	t.Helper()
	rows, err := st.TraceRepo().List(context.Background(), probeDate)
	if err != nil {
		t.Fatalf("读 run_trace 失败: %v", err)
	}
	subject := model.TraceAlert(code)
	for _, r := range rows {
		if r.Subject == subject {
			return r, true
		}
	}
	return model.RunTrace{}, false
}

// TestJobFailedAlertSurvivesLaterSuccess 钉四时刻方案的承重假设（组长 20261119 验收硬项①）：
//
// 配多个时刻后，重跑成功会把 job:<任务> 那一行的 fail 整行盖掉
// （见 fired_single_at_test.go 的 TestMultiAtRerunOverwritesOneTraceRow）；但首跑失败当时
// 由 cmd/jingzhe/main.go 装配的 OnJobDone 另落了一行 alert:JOB_FAILED:<任务> —— 成功那一轮
// outcome != fail，既不报警也不写这个 subject，所以首跑的原文与时刻在库里永久留底。
//
// 这条不成立就不能用四时刻方案（"当天到底失败过没有"会变成不可回溯），应当场停手回方案 B。
func TestJobFailedAlertSurvivesLaterSuccess(t *testing.T) {
	const failReason = "① 行情同步: tushare daily 失败 code=50101(transient) msg=dial tcp"
	var runs int
	s, st := newProbe(t, JobSpec{
		Name: JobEveningPipeline, At: []string{"16:30", "17:10"}, TradeDayOnly: true,
		Run: func(context.Context, *observability.RunCtx) error {
			runs++
			if runs == 1 {
				return errors.New(failReason)
			}
			return nil // 第二枪上游已恢复
		},
	})
	// 复刻生产装配的完成回调；mailer 传 nil —— 本用例只验留痕面，不发信（Raise 内有判空）。
	alerts := notify.NewAlertService(st, nil)
	s = s.WithOnDone(func(spec JobSpec, tradeDate, outcome string, jobErr error) {
		if outcome != model.TraceFail {
			return
		}
		reason := ""
		if jobErr != nil {
			reason = jobErr.Error()
		}
		if err := alerts.Raise(context.Background(), tradeDate, "scheduler", model.AlertUrgent,
			"JOB_FAILED:"+spec.Name, "任务失败: "+spec.Name, reason); err != nil {
			t.Fatalf("落 JOB_FAILED 告警失败: %v", err)
		}
	})
	// 等价于生产中 16:32→17:10 的 38 分钟（本就超出 30 分钟冷却），不是人为放宽条件。
	s.cooldown = 0

	s.Tick(context.Background(), clockAt("16:30"))
	before, ok := alertRow(t, st, "JOB_FAILED:"+JobEveningPipeline)
	if !ok || before.Outcome != model.TraceFail || !strings.Contains(before.Detail, failReason) {
		t.Fatalf("首跑失败当场应有 alert:JOB_FAILED 行且写明原因，实际 ok=%v %+v", ok, before)
	}

	s.Tick(context.Background(), clockAt("17:10"))
	if runs != 2 {
		t.Fatalf("第二个时刻应补跑，实际执行 %d 次", runs)
	}

	if rows := traceRowsFor(t, st, JobEveningPipeline); len(rows) != 1 || rows[0].Outcome != model.TraceOK {
		t.Fatalf("期望 job 行被重跑盖成 ok，实际 %+v", rows)
	}
	after, ok := alertRow(t, st, "JOB_FAILED:"+JobEveningPipeline)
	if !ok {
		t.Fatal("重跑成功后 alert:JOB_FAILED 行整行消失，承重假设不成立")
	}
	if after.Outcome != model.TraceFail || after.Detail != before.Detail || after.At != before.At {
		t.Errorf("后续成功改写了 alert:JOB_FAILED 行（应回方案 B）：before outcome=%s at=%s detail=%q / after outcome=%s at=%s detail=%q",
			before.Outcome, before.At, before.Detail, after.Outcome, after.At, after.Detail)
	}
}

// TestTraceDetailCarriesTriggerNote 验 P2：job 行 detail 末尾追加触发来源与时刻，
// 而前面的失败原因/降级说明一字不动 —— 多时刻共用一行后要能看出这行是哪一次执行留下的。
//
// 只做观测：outcome 判定（HasSucceeded）、冷却判定（LastAt）、告警（另一 subject）都不读这段文本。
func TestTraceDetailCarriesTriggerNote(t *testing.T) {
	const failReason = "① 行情同步: tushare daily 失败 code=50101(transient)"
	var runs int
	s, st := newProbe(t, JobSpec{
		Name: "note_probe", At: []string{"16:30"}, TradeDayOnly: true,
		Run: func(context.Context, *observability.RunCtx) error {
			runs++
			if runs == 1 {
				return errors.New(failReason)
			}
			return nil
		},
	})

	s.Tick(context.Background(), clockAt("16:30"))
	row, ok := traceOf(t, st, "note_probe")
	if !ok {
		t.Fatal("首跑失败应留 job 行")
	}
	want := failReason + triggerNoteSep + "触发=scheduler@16:30"
	if row.Detail != want {
		t.Errorf("fail 行 detail = %q，期望 %q（原文不动 + 末尾触发标注）", row.Detail, want)
	}

	// 手工触发路径：RunNamed 的 manual=true，来源写 manual、不带 @时刻（trigger 本身就是 "manual"）。
	if err := s.RunNamed(context.Background(), "note_probe", probeDate, "manual"); err != nil {
		t.Fatalf("手工触发失败: %v", err)
	}
	row, _ = traceOf(t, st, "note_probe")
	if row.Outcome != model.TraceOK || row.Detail != "触发=manual" {
		t.Errorf("手工重跑后 detail = %q（outcome=%s），期望正文只剩「触发=manual」", row.Detail, row.Outcome)
	}

	// 邮件结论读回时按 stripTriggerNote 剔标注：ok 且正文为空仍判"正常跑完"，
	// 不能被这条观测文本带偏（dayparts.go tradeConclusion 的判据）。
	if got := stripTriggerNote("触发=scheduler@16:30"); got != "" {
		t.Errorf("stripTriggerNote 纯标注 = %q，期望空串", got)
	}
	if got := stripTriggerNote(want); got != failReason {
		t.Errorf("stripTriggerNote 带原因 = %q，期望 %q", got, failReason)
	}
	if got := stripTriggerNote("降级 SCREEN_EMPTY: 关闸"); got != "降级 SCREEN_EMPTY: 关闸" {
		t.Errorf("stripTriggerNote 误伤无标注正文: %q", got)
	}
}
