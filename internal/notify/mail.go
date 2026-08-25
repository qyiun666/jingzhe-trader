package notify

import (
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"

	"jingzhe-trader/pkg/logger"
)

// ==================== 邮件发送器 ====================

// mailSMTPAddr QQ 邮箱 SMTP 服务器 (465 隐式 TLS)
const mailSMTPAddr = "smtp.qq.com:465"

// mailSMTPHost QQ 邮箱 SMTP 主机名 (用于 TLS ServerName 与 EHLO)
const mailSMTPHost = "smtp.qq.com"

// MailNotifier QQ 邮箱邮件通知器
// 未启用时所有发送调用降级为 no-op, 调用方无需判空
type MailNotifier struct {
	enabled  bool   // 是否启用邮件通知
	from     string // 发件邮箱 (即收件人)
	password string // SMTP 授权码 (仅环境变量注入, 不落盘)
}

// NewMailNotifier 创建邮件通知器
func NewMailNotifier(enabled bool, from, password string) *MailNotifier {
	return &MailNotifier{
		enabled:  enabled,
		from:     from,
		password: password,
	}
}

// Enabled 是否已完整配置 (开关 + 发件人 + 授权码)
func (n *MailNotifier) Enabled() bool {
	return n != nil && n.enabled && n.from != "" && n.password != ""
}

// Send 发送纯文本邮件 (主题=title, 收件人=发件人)
func (n *MailNotifier) Send(title, text string) error {
	return n.send(title, text, false)
}

// SendHTML 发送 HTML 邮件 (主题=title, 收件人=发件人)
// 用于盘前总结/日报等需要排版的通知, 内容必须为完整或片段 HTML
func (n *MailNotifier) SendHTML(title, htmlBody string) error {
	return n.send(title, htmlBody, true)
}

// send 统一发送入口
func (n *MailNotifier) send(title, body string, html bool) error {
	if !n.Enabled() {
		return nil
	}
	msg := buildMailMessage(n.from, title, body)
	if html {
		msg = buildHTMLMailMessage(n.from, title, body)
	}
	if err := sendSMTP(mailSMTPAddr, mailSMTPHost, true, n.from, n.password, n.from, msg); err != nil {
		return fmt.Errorf("发送邮件失败: %w", err)
	}
	logger.L().Debugf("[Mail] 邮件发送成功")
	return nil
}

// buildMailMessage 构建纯文本邮件 (RFC 5322 最小集, 正文含标题行)
func buildMailMessage(from, title, text string) string {
	return buildMailMessageWithContentType(from, title, title+"\n\n"+text, "text/plain; charset=UTF-8")
}

// buildHTMLMailMessage 构建 HTML 邮件 (RFC 5322 最小集)
func buildHTMLMailMessage(from, title, htmlBody string) string {
	return buildMailMessageWithContentType(from, title, htmlBody, "text/html; charset=UTF-8")
}

// buildMailMessageWithContentType 构建邮件 (RFC 5322 最小集)
func buildMailMessageWithContentType(from, title, body, contentType string) string {
	// Subject 含换行会破坏邮件头结构, 统一压平
	flatTitle := strings.NewReplacer("\r", " ", "\n", " ").Replace(title)
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", from)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", flatTitle))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(&b, "Content-Type: %s\r\n", contentType)
	b.WriteString("\r\n")
	b.WriteString(body)
	return b.String()
}

// sendSMTP 通过 SMTP 发送邮件
// useTLS=true 时先做隐式 TLS 握手 (QQ 465); false 时明文直连 (测试用)
func sendSMTP(addr, host string, useTLS bool, from, password, to, msg string) error {
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	if err != nil {
		return fmt.Errorf("连接SMTP服务器失败: %w", err)
	}
	defer conn.Close()

	var rwc net.Conn = conn
	if useTLS {
		tconn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tconn.Handshake(); err != nil {
			return fmt.Errorf("TLS握手失败: %w", err)
		}
		rwc = tconn
	}

	c, err := smtp.NewClient(rwc, host)
	if err != nil {
		return fmt.Errorf("SMTP握手失败: %w", err)
	}
	defer c.Close()

	if err := c.Auth(smtp.PlainAuth("", from, password, host)); err != nil {
		return fmt.Errorf("SMTP认证失败: %w", err)
	}
	if err := c.Mail(from); err != nil {
		return fmt.Errorf("MAIL FROM失败: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO失败: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA失败: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("结束邮件内容失败: %w", err)
	}
	return c.Quit()
}
