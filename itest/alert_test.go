package itest

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestUrgentAlertMailOnJobFailure "任务失败 → 一封真邮件"这条链路。
//
// 盯盘与常驻服务靠它把异常推到人面前：跑一个必然失败的任务（1991 年日历里没有任何
// 交易日，日线同步那一步就该报错），再按 serve/cli 相同的口径把失败转成 urgent 告警，
// 断言当日既留下 alert:JOB_FAILED 的 fail 轨迹，也留下投递成功的 mail:M6 轨迹。
func TestUrgentAlertMailOnJobFailure(t *testing.T) {
	rt, _ := requireRuntime(t)
	ctx := t.Context()
	const badDate = "19910101"

	err := rt.RunTaskOnce(ctx, "evening_pipeline", badDate, "itest")
	if err == nil {
		t.Fatal("日历里没有 1991 年的交易日，流水线却返回了成功")
	}
	if !strings.Contains(err.Error(), "日历") {
		t.Errorf("失败原因应当指向日历缺失，实际: %v", err)
	}
	assertJobOutcome(t, rt.Store, badDate, "evening_pipeline", model.TraceFail)

	// 与 cmd/jingzhe 两条触发路径同一口径：失败 → Raise(urgent) → M6 真投递。
	if rerr := rt.Alerts.Raise(ctx, badDate, "itest", model.AlertUrgent,
		"JOB_FAILED", "任务失败: evening_pipeline", err.Error()); rerr != nil {
		t.Fatalf("urgent 告警上抛失败：这条错误一旦被人看不见，就是盯盘出事没人知道: %v", rerr)
	}
	rows, terr := rt.Store.TraceRepo().List(ctx, badDate)
	mustNoErr(t, "读轨迹", terr)
	var alertRow, mailRow bool
	for _, r := range rows {
		switch r.Subject {
		case model.TraceAlert("JOB_FAILED"):
			alertRow = r.Outcome == model.TraceFail && strings.Contains(r.Detail, "urgent")
		case model.TraceMail(model.MailM6):
			mailRow = r.Outcome == model.TraceOK
			t.Log(describe("告警邮件轨迹", r.Detail))
		}
	}
	if !alertRow {
		t.Error("alert:JOB_FAILED 没留下带 urgent 的 fail 轨迹")
	}
	if !mailRow {
		t.Error("urgent 告警没有真的发出 M6 邮件")
	}
}

// TestRunNamedRefusesUnknownTask 未知任务名必须显式拒绝，而不是"没找到就当跑过了"。
func TestRunNamedRefusesUnknownTask(t *testing.T) {
	rt, _ := requireRuntime(t)
	err := rt.RunTaskOnce(t.Context(), "not_a_task", "20260903", "itest")
	if err == nil || !strings.Contains(err.Error(), "未知任务") {
		t.Fatalf("应当报未知任务，实际: %v", err)
	}
	if got := rt.TaskNames(); len(got) != 5 {
		t.Errorf("注册任务数=%d（%v），期望 5 个触发点", len(got), got)
	}
}
