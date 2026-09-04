package notify

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// newTestMailer 构造一个只统计投递次数的 Mailer：真的 SMTP 在单测里不该被碰到。
func newTestMailer(t *testing.T, name string) (*Mailer, *store.Store, *int) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sent := 0
	m := NewMailer(st, MailConfig{
		Enabled: true,
		SMTP:    SMTPConfig{Host: "smtp.example.com", Port: 465, From: "me@example.com", Password: "x"},
		To:      []string{"you@example.com"},
	})
	m.sendFn = func([]string, string, string) error { sent++; return nil }
	return m, st, &sent
}

// TestMailerOnceDailySendsOnePerDay M1/M2/M5 当日一封。
//
// 实测复现过的成因：外部 agent 用 trigger_task 手工补跑 morning_plan，
// 每补跑一次就往收件箱塞一封同样的 M2；而 run_trace 按 (交易日, subject) 覆盖成一行，
// 查日志根本看不出发了几封。去重后任务照常算完成，只是不再重复投递。
func TestMailerOnceDailySendsOnePerDay(t *testing.T) {
	m, st, sent := newTestMailer(t, "once.db")
	ctx := t.Context()
	const date = "20260904"

	for i := 1; i <= 3; i++ {
		if err := m.Send(ctx, date, model.MailM2, "主题", "正文"); err != nil {
			t.Fatalf("第 %d 次 Send 失败: %v", i, err)
		}
	}
	if *sent != 1 {
		t.Errorf("M2 当日投了 %d 次，期望 1 次（OnceDaily 类型重复补跑不该再发）", *sent)
	}
	// 跳过重复发送不能被当成失败：轨迹行必须还是 ok，否则任务会被判砸
	ok, err := st.TraceRepo().HasSucceeded(ctx, model.TraceMail(model.MailM2), date)
	if err != nil {
		t.Fatalf("查轨迹失败: %v", err)
	}
	if !ok {
		t.Error("M2 轨迹行不是 ok —— 补跑会被判成投递失败")
	}

	// 换一个交易日就该重新投递：去重窗口是"当日"，不是"永久"
	if err := m.Send(ctx, "20260907", model.MailM2, "主题", "正文"); err != nil {
		t.Fatalf("次日 Send 失败: %v", err)
	}
	if *sent != 2 {
		t.Errorf("次日 M2 投了 %d 次（累计），期望 2 —— 去重按交易日各算一次", *sent)
	}
}

// TestMailerRepeatableTypesNotDeduped M3（盘中止损，每轮内容不同）与
// M6（告警，一天可多条）的重复有信息量，不能被那道去重误伤。
func TestMailerRepeatableTypesNotDeduped(t *testing.T) {
	m, _, sent := newTestMailer(t, "repeat.db")
	ctx := t.Context()
	const date = "20260904"

	for i := 0; i < 3; i++ {
		if err := m.Send(ctx, date, model.MailM3, "止损", "第二轮又跌破一条线"); err != nil {
			t.Fatalf("M3 Send 失败: %v", err)
		}
	}
	if *sent != 3 {
		t.Errorf("M3 当日投了 %d 次，期望 3 次（每轮止损都要发）", *sent)
	}
}

// TestAlertUrgentMailsOncePerCodePerDay 同一个 urgent 告警码当天只发一封。
//
// 以前 urgent 完全绕过去重，而 JOB_FAILED 这类反复触发的码每轮都发一封。
// 现在轨迹行每次照常刷新（保留最新原因与时间），只是不再重复投递。
func TestAlertUrgentMailsOncePerCodePerDay(t *testing.T) {
	m, st, sent := newTestMailer(t, "alert.db")
	ctx := t.Context()
	const date = "20260904"

	a := NewAlertService(st, m)
	raise := func(why string) {
		if err := a.Raise(ctx, date, "scheduler", model.AlertUrgent, "JOB_FAILED:intraday_scan", "任务失败", why); err != nil {
			t.Fatalf("Raise 失败: %v", err)
		}
	}
	raise("第一次：取价失败")
	raise("第二次：还是取价失败")
	if *sent != 1 {
		t.Errorf("同一 urgent code 当日发了 %d 封，期望 1 封", *sent)
	}

	// 轨迹必须反映最近一次：只留第一次的原因会把新故障藏起来
	rows, err := st.TraceRepo().List(ctx, date)
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	var found *model.RunTrace
	for i := range rows {
		if rows[i].Subject == model.TraceAlert("JOB_FAILED:intraday_scan") {
			found = &rows[i]
		}
	}
	if found == nil {
		t.Fatal("重复触发后轨迹行不见了（应当每次刷新而不是丢弃）")
	}
	if !strings.HasSuffix(found.Detail, "第二次：还是取价失败") {
		t.Errorf("轨迹 detail 没保留最近一次原因: %q", found.Detail)
	}
	if found.Outcome != model.TraceFail {
		t.Errorf("告警轨迹 outcome=%q，期望 %q", found.Outcome, model.TraceFail)
	}

	// 不同 code 各算各的：一个任务先报过，不该把另一个任务的失败也吞掉
	if err := a.Raise(ctx, date, "scheduler", model.AlertUrgent, "JOB_FAILED:daily_report", "任务失败", "日报砸了"); err != nil {
		t.Fatalf("第二个 code Raise 失败: %v", err)
	}
	if *sent != 2 {
		t.Errorf("不同 code 的第二条 urgent 投了 %d 封（累计），期望 2 —— 去重必须按 code 分", *sent)
	}
}

// TestAlertNonUrgentStillUsesHourWindow 非 urgent 的 1 小时窗口不能被这次改动带偏。
func TestAlertNonUrgentStillUsesHourWindow(t *testing.T) {
	m, st, sent := newTestMailer(t, "warn.db")
	ctx := t.Context()
	a := NewAlertService(st, m)
	fixed := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return fixed }

	for i := 0; i < 2; i++ {
		if err := a.Raise(ctx, "20260904", "screener", model.AlertWarning, "SOME_WARN", "标题", "内容"); err != nil {
			t.Fatalf("Raise 失败: %v", err)
		}
	}
	if *sent != 0 {
		t.Errorf("warning 级告警发了 %d 封信，期望 0（只有 urgent 才投递）", *sent)
	}
	rows, err := st.TraceRepo().List(ctx, "20260904")
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Subject == model.TraceAlert("SOME_WARN") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("warning 轨迹行数 %d，期望 1（1 小时内去重）", n)
	}
}
