package itest

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
)

// TestSMTPDelivery 真发一封 M6 告警邮件到 watch.mail_to，并核对发信轨迹真的落了。
//
// 这是"盯盘发现紧急情况能发邮件"这条验收的最小可执行证据：
// 只测 RenderM6 的字符串是测不出授权码过期、465 端口不通、QQ 拒收的。
func TestSMTPDelivery(t *testing.T) {
	st, cfg := requireEnv(t)
	ctx := t.Context()
	to := mailTo(t, cfg)
	date := tradeDateOr(t, "20260903")

	mail := notify.NewMailer(st, app.MailConfigOf(cfg))
	if missing := mail.HealthOK(); len(missing) > 0 {
		t.Fatalf("邮件配置不完整: %v", missing)
	}
	subject, body := notify.RenderM6("urgent", "ITEST_SMTP", "集成测试：紧急邮件通道",
		"这封来自 go test ./itest，用于验证盘中紧急告警能真的投递。可删除。")
	if !strings.Contains(subject, "ITEST_SMTP") {
		t.Fatalf("M6 主题里应带告警码，实际: %q", subject)
	}
	if err := mail.Send(ctx, date, model.MailM6, subject, body); err != nil {
		t.Fatalf("真实发信失败: %v", err)
	}

	traces, err := st.TraceRepo().List(ctx, date)
	mustNoErr(t, "读发信轨迹", err)
	var found *model.RunTrace
	for i := range traces {
		if traces[i].Subject == model.TraceMail(model.MailM6) {
			found = &traces[i]
		}
	}
	if found == nil {
		t.Fatalf("发信成功却没有 mail 轨迹行（当日 %d 行轨迹里没有 %s）",
			len(traces), model.TraceMail(model.MailM6))
	}
	if found.Outcome != model.TraceOK {
		t.Errorf("发信轨迹 outcome=%q detail=%q，期望 ok", found.Outcome, found.Detail)
	}
	t.Log(describe("收件人", to, "轨迹", found.Detail))
}

// TestSMTPFailureIsVisible 配置不全时 Send 必须报错并留一行 fail 轨迹，
// 不能"静默不发还返回 nil"——那正是本系统历史上"任务全绿零邮件"的成因。
func TestSMTPFailureIsVisible(t *testing.T) {
	st, cfg := requireEnv(t)
	ctx := t.Context()
	date := tradeDateOr(t, "20260903")

	broken := app.MailConfigOf(cfg)
	broken.To = nil // 收件人为空
	mail := notify.NewMailer(st, broken)
	err := mail.Send(ctx, date, model.MailM1, "[集成测试] 空收件人", "body")
	if err == nil {
		t.Fatal("空收件人时 Send 返回了 nil，邮件失败不可见")
	}
	if !strings.Contains(err.Error(), "mail.to") && !strings.Contains(err.Error(), "收件人") {
		t.Errorf("错误信息没说到收件人缺失，实际: %v", err)
	}
	traces, terr := st.TraceRepo().List(ctx, date)
	mustNoErr(t, "读轨迹", terr)
	var failRow bool
	for _, tr := range traces {
		if tr.Subject == model.TraceMail(model.MailM1) && tr.Outcome == model.TraceFail {
			failRow = true
		}
	}
	if !failRow {
		t.Errorf("发送失败应留一行 fail 轨迹，实际当日轨迹 %d 行里没有", len(traces))
	}
}
