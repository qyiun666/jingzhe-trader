package notify

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTPConfig SMTP 连接配置（QQ 邮箱 465 隐式 TLS，附录 A）。
type SMTPConfig struct {
	Host     string
	Port     int
	From     string // 发件邮箱（同为 SMTP 登录账号）
	Password string // 授权码（非登录密码）
}

// Address 返回 host:port。
func (c SMTPConfig) Address() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

// loginAuth SMTP LOGIN 认证（QQ 邮箱对 PLAIN 兼容性差，实测 LOGIN 稳定）。
type loginAuth struct {
	username string
	password string
}

func (l *loginAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

func (l *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:", "user name:", "用户名:":
		return []byte(l.username), nil
	case "password:", "密码:":
		return []byte(l.password), nil
	default:
		return nil, fmt.Errorf("未知 SMTP LOGIN 提示: %q", fromServer)
	}
}

// Send 通过隐式 TLS SMTP 发送纯文本邮件。绝对超时 90s（附录 A）。
func (c SMTPConfig) Send(to []string, subject, body string) error {
	if err := c.validate(to); err != nil {
		return err
	}

	dialer := &net.Dialer{Timeout: 90 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", c.Address(), &tls.Config{ServerName: c.Host})
	if err != nil {
		return fmt.Errorf("连接 SMTP %s 失败: %w", c.Address(), err)
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("建立 SMTP 会话失败: %w", err)
	}
	defer client.Close()

	// 认证：优先 LOGIN（QQ 邮箱），失败回退 PLAIN
	if ok, _ := client.Extension("AUTH"); ok {
		if aerr := client.Auth(&loginAuth{username: c.From, password: c.Password}); aerr != nil {
			if perr := client.Auth(smtp.PlainAuth("", c.From, c.Password, c.Host)); perr != nil {
				return fmt.Errorf("SMTP 认证失败: login=%v plain=%v", aerr, perr)
			}
		}
	}

	if err := client.Mail(c.From); err != nil {
		return fmt.Errorf("SMTP MAIL FROM 失败: %w", err)
	}
	for _, rcpt := range to {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("SMTP RCPT TO %s 失败: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA 失败: %w", err)
	}
	if _, err := w.Write(buildMessage(c.From, to, subject, body, client)); err != nil {
		_ = w.Close()
		return fmt.Errorf("写入邮件内容失败: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("结束邮件内容失败: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("SMTP QUIT 失败: %w", err)
	}
	return nil
}

func (c SMTPConfig) validate(to []string) error {
	if c.Host == "" {
		return fmt.Errorf("SMTP 配置不完整：host 为空")
	}
	if c.From == "" {
		return fmt.Errorf("SMTP 配置不完整：发件人为空")
	}
	if c.Password == "" {
		return fmt.Errorf("SMTP 配置不完整：授权码为空")
	}
	if len(to) == 0 {
		return fmt.Errorf("SMTP 配置不完整：收件人为空")
	}
	return nil
}

// buildMessage 构造 RFC 5322 消息：
//   - 中文主题走 RFC 2047 Q 编码；
//   - 正文先按字节折行（≤990，不切 UTF-8），服务器支持 8BITMIME 则 8bit 直发，
//     否则回退 base64（76 字符行，天然满足行宽上限）。
func buildMessage(from string, to []string, subject, body string, client *smtp.Client) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")

	folded := FoldBody(body)
	if ok, _ := client.Extension("8BITMIME"); ok {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(folded)
	} else {
		b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		enc := base64.StdEncoding.EncodeToString([]byte(folded))
		for len(enc) > 76 {
			b.WriteString(enc[:76])
			b.WriteString("\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc)
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}
