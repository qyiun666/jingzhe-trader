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

// mailMaxLineLen 正文单行最大字节数 (RFC 5321 限制 998 octets, QQ 邮箱超限拒收; 留余量)
// 按字节而非字符计数: UTF-8 中文 1 字符=3 字节, 按字符折叠会突破 998 字节上限
const mailMaxLineLen = 990

// MailNotifier QQ 邮箱邮件通知器
// 未启用时所有发送调用降级为告警日志+no-op, 调用方无需判空
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
		logger.L().Warnw("邮件通知未启用, 跳过发送", "title", title)
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
	b.WriteString(foldLongLines(body, mailMaxLineLen))
	return b.String()
}

// foldLongLines 折叠超过 maxBytes 个字节的行 (RFC 5321 单行 ≤998 octets, 避免 QQ 邮箱拒收)
// HTML/JS 中插入换行会被解析为空白, 不影响渲染; 按字节折叠并修正 UTF-8 边界避免切断多字节字符
func foldLongLines(body string, maxBytes int) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if len(line) <= maxBytes {
			continue
		}
		lines[i] = foldLine(line, maxBytes)
	}
	return strings.Join(lines, "\n")
}

// foldLine 单行折叠: 优先在空格处断行, 其次在标签闭合符 > 之后 (避免切断 HTML 标签), 最后硬折; 折行后跳过行首空格
// 切割位置按字节计算, 并用 prevRuneBoundary 修正到 UTF-8 字符边界
func foldLine(line string, maxBytes int) string {
	if len(line) <= maxBytes {
		return line
	}
	var b strings.Builder
	rest := line
	for len(rest) > maxBytes {
		cut := maxBytes
		for i := maxBytes - 1; i > 0; i-- {
			if rest[i] == ' ' {
				cut = i
				break
			}
		}
		if cut == maxBytes { // 无空格: 找标签闭合符, 在其后断行
			for i := maxBytes - 1; i > 0; i-- {
				if rest[i] == '>' {
					cut = i + 1
					break
				}
			}
		}
		cut = prevRuneBoundary(rest, cut) // 切割点回退到字符边界, 不切断多字节 UTF-8
		b.WriteString(rest[:cut])
		b.WriteByte('\n')
		rest = rest[cut:]
		for len(rest) > 0 && rest[0] == ' ' {
			rest = rest[1:]
		}
	}
	b.WriteString(rest)
	return b.String()
}

// prevRuneBoundary 将 pos 向前回退到 UTF-8 字符边界 (pos 落在多字节字符中间时)
// UTF-8 连续字节形如 10xxxxxx, 向前跳过即可到达字符起始字节
func prevRuneBoundary(s string, pos int) int {
	for pos > 0 && pos < len(s) && s[pos]&0xC0 == 0x80 {
		pos--
	}
	return pos
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
