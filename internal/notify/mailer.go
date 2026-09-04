package notify

import (
	"context"
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// MailConfig 邮件通道配置。
type MailConfig struct {
	Enabled bool
	SMTP    SMTPConfig
	To      []string // 收件人（可多个）
}

// Mailer 邮件通道：同步发一次，结果落一行轨迹。
//
// 原来的发件箱表（pending / attempts / next_retry_at 三级退避 + 跨进程补发）已删除 ——
// 那是为"SMTP 半夜挂了早上自动补上"设计的，代价是一整张状态机表和一个每轮扫描的定时器。
// 现在发信只有一次机会：成了写 ok，砸了写 fail 并当场把错误上抛给调用方，
// 当日轨迹里看得见，不再悄悄排队。
type Mailer struct {
	st     *store.Store
	cfg    MailConfig
	now    func() time.Time
	sendFn func(to []string, subject, body string) error // 可注入（测试打桩）
}

// NewMailer 构造邮件通道。
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

// Configured 是否具备发送条件（启用 + 收件人 + 发件人）。
func (m *Mailer) Configured() bool {
	return m.cfg.Enabled && len(m.cfg.To) > 0 && m.cfg.SMTP.From != ""
}

// Send 同步发送一封邮件，成败都落一行轨迹（subject = mail:<类型>）并显式返回错误。
//
// 未配置【不是 no-op】：照样写一行 fail 轨迹并返回 ErrNotConfigured 包装错误（验收 §10.5-5），
// 否则"任务全绿但零邮件"那个历史缺陷（D1）会重新变成不可见。
//
// OnceDaily 类型（M1/M2/M5）当日已投递成功过就跳过：`trigger_task` 手工补跑同一个任务
// 不该往收件箱再塞一封，而 run_trace 按 (trade_date, subject) 覆盖成一行，重复发放在轨迹里看不出来。
func (m *Mailer) Send(ctx context.Context, tradeDate string, typ model.MailType, subject, body string) error {
	if typ.OnceDaily() {
		done, err := m.st.TraceRepo().HasSucceeded(ctx, model.TraceMail(typ), tradeDate)
		if err != nil {
			return fmt.Errorf("查询 %s 邮件当日投递状态失败: %w", typ, err)
		}
		if done {
			observability.S().Infow("邮件当日已送达，跳过重复发送", "type", string(typ), "date", tradeDate)
			return nil
		}
	}
	if !m.Configured() {
		reason := "mail.enabled=false"
		if m.cfg.Enabled && len(m.cfg.To) == 0 {
			reason = "收件人 mail.to 未配置"
		}
		m.trace(ctx, tradeDate, typ, false, "未发送："+reason)
		return fmt.Errorf("发送 %s 邮件失败: %w（%s）", typ, ErrNotConfigured, reason)
	}
	if serr := m.sendFn(m.cfg.To, subject, body); serr != nil {
		m.trace(ctx, tradeDate, typ, false, "SMTP 发送失败: "+serr.Error())
		return fmt.Errorf("发送 %s 邮件失败: %w", typ, serr)
	}
	m.trace(ctx, tradeDate, typ, true, "已发送至 "+m.cfg.To[0])
	observability.S().Infow("邮件已发送", "type", string(typ), "date", tradeDate)
	return nil
}

// trace 落一行发信轨迹。轨迹写失败不掩盖发信结果，只额外记一条日志。
func (m *Mailer) trace(ctx context.Context, tradeDate string, typ model.MailType, ok bool, detail string) {
	outcome := model.TraceOK
	if !ok {
		outcome = model.TraceFail
	}
	err := m.st.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: tradeDate,
		Subject:   model.TraceMail(typ),
		Outcome:   outcome,
		Detail:    detail,
		At:        m.now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		observability.S().Errorw("写发信轨迹失败", "type", string(typ), "err", err)
	}
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
