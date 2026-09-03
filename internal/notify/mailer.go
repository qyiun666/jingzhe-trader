package notify

import (
	"context"
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// retryBackoffs 发送失败重试间隔：1 / 5 / 15 分钟（附录 A）。
var retryBackoffs = []time.Duration{1 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// maxSendAttempts 最大发送尝试次数（3 次后终态 failed）。
const maxSendAttempts = 3

// MailConfig 邮件通道配置。
type MailConfig struct {
	Enabled bool
	SMTP    SMTPConfig
	To      []string // 收件人（可多个）
}

// Mailer 邮件发件箱：Enqueue 落库 → Send（SMTP）→ 状态回写 → 重试退避。
type Mailer struct {
	st     *store.Store
	cfg    MailConfig
	now    func() time.Time
	sendFn func(to []string, subject, body string) error // 可注入（测试打桩）
}

// NewMailer 构造邮件发件箱。
func NewMailer(st *store.Store, cfg MailConfig) *Mailer {
	return &Mailer{st: st, cfg: cfg, now: time.Now, sendFn: cfg.SMTP.Send}
}

// WithClock 注入时钟（测试用）。
func (m *Mailer) WithClock(f func() time.Time) *Mailer {
	if f != nil {
		m.now = f
	}
	return m
}

// WithSender 注入发送函数（测试打桩，禁止单测打真实 SMTP）。
func (m *Mailer) WithSender(f func(to []string, subject, body string) error) *Mailer {
	if f != nil {
		m.sendFn = f
	}
	return m
}

// Configured 是否具备发送条件（启用 + 收件人 + 发件人）。
func (m *Mailer) Configured() bool {
	return m.cfg.Enabled && len(m.cfg.To) > 0 && m.cfg.SMTP.From != ""
}

// Enqueue 入队邮件。
//
// 未启用时【不是 no-op】：显式落 mail_outbox failed 行（last_error 写明原因），
// 并返回 ErrNotConfigured 包装错误（验收 §10.5-5）。调用方必须处理该错误。
func (m *Mailer) Enqueue(ctx context.Context, tradeDate string, typ model.MailType, subject, body string) (int64, error) {
	nowStr := m.now().UTC().Format(time.RFC3339)
	if !m.Configured() {
		reason := "mail.enabled=false"
		if m.cfg.Enabled && len(m.cfg.To) == 0 {
			reason = "收件人 mail.to 未配置"
		}
		id, err := m.st.OpsRepo().InsertMail(ctx, model.MailOutbox{
			TradeDate: tradeDate, MailType: typ, Subject: subject, Body: body,
			Status: "failed", Attempts: 0, LastError: reason, CreatedAt: nowStr,
		})
		if err != nil {
			return 0, fmt.Errorf("落邮件失败行失败: %w", err)
		}
		return id, fmt.Errorf("入队 %s 邮件失败: %w（%s，已落 mail_outbox failed 行）", typ, ErrNotConfigured, reason)
	}
	return m.st.OpsRepo().EnqueueMail(ctx, model.MailOutbox{
		TradeDate: tradeDate, MailType: typ, Subject: subject, Body: body, CreatedAt: nowStr,
	})
}

// SendNow 入队并立即发送（urgent 邮件 / RaiseUrgent 用）。
// 发送失败返回显式错误（重试交给 SendPending 的退避机制）。
func (m *Mailer) SendNow(ctx context.Context, tradeDate string, typ model.MailType, subject, body string) error {
	id, err := m.Enqueue(ctx, tradeDate, typ, subject, body)
	if err != nil {
		return err
	}
	return m.SendOne(ctx, id)
}

// SendOne 发送单封邮件并回写状态：成功 → sent；失败 → attempts+1 + 退避，
// 耗尽 3 次后终态 failed。错误一律显式上抛（不静默）。
func (m *Mailer) SendOne(ctx context.Context, id int64) error {
	row, err := m.st.OpsRepo().GetMail(ctx, id)
	if err != nil {
		return err
	}
	if row.Status == "sent" {
		return nil // 幂等：已发送不重发
	}
	nowStr := m.now().UTC().Format(time.RFC3339)
	if serr := m.sendFn(m.cfg.To, row.Subject, row.Body); serr != nil {
		attempts := row.Attempts + 1
		if attempts >= maxSendAttempts {
			if ferr := m.st.OpsRepo().MarkMailFailed(ctx, id, attempts, serr.Error()); ferr != nil {
				return fmt.Errorf("发送失败且回写终态失败: %v / %w", serr, ferr)
			}
			return fmt.Errorf("邮件 #%d 发送失败（已重试 %d 次，终态 failed）: %w", id, attempts, serr)
		}
		next := m.now().Add(retryBackoffs[min(attempts, len(retryBackoffs))-1]).UTC().Format(time.RFC3339)
		if rerr := m.st.OpsRepo().UpdateMailRetry(ctx, id, attempts, serr.Error(), next); rerr != nil {
			return fmt.Errorf("发送失败且回写重试状态失败: %v / %w", serr, rerr)
		}
		return fmt.Errorf("邮件 #%d 发送失败（第 %d 次，%s 前不重试）: %w", id, attempts, next, serr)
	}
	if serr := m.st.OpsRepo().UpdateMailSent(ctx, id, nowStr); serr != nil {
		return fmt.Errorf("回写邮件 #%d 已发送失败: %w", id, serr)
	}
	observability.S().Infow("邮件已发送", "id", id, "type", string(row.MailType), "date", row.TradeDate)
	return nil
}

// SendPending 发送所有到期待发邮件，返回成功条数与最后一个错误（显式，不吞）。
func (m *Mailer) SendPending(ctx context.Context, tradeDate string) (int, error) {
	rows, err := m.st.OpsRepo().PendingMails(ctx, m.now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	sent := 0
	var lastErr error
	for _, row := range rows {
		if tradeDate != "" && row.TradeDate != tradeDate {
			continue
		}
		if err := m.SendOne(ctx, row.ID); err != nil {
			lastErr = err
			continue
		}
		sent++
	}
	return sent, lastErr
}

// HealthOK 配置完整性自检（不发信）：返回缺失项清单，空切片 = 健康。
func (m *Mailer) HealthOK() []string {
	var missing []string
	if !m.cfg.Enabled {
		return []string{"mail.enabled=false（邮件关闭，发送将显式失败）"}
	}
	if m.cfg.SMTP.Host == "" {
		missing = append(missing, "mail.smtp_host")
	}
	if m.cfg.SMTP.From == "" {
		missing = append(missing, "mail.from")
	}
	if m.cfg.SMTP.Password == "" {
		missing = append(missing, "mail.password")
	}
	if len(m.cfg.To) == 0 {
		missing = append(missing, "mail.to")
	}
	return missing
}
