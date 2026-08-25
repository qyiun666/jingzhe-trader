package notify

import (
	"bufio"
	"net"
	"strings"
	"sync"
	"testing"
)

func TestMailNotifierEnabled(t *testing.T) {
	cases := []struct {
		name string
		n    *MailNotifier
		want bool
	}{
		{"nil接收者", nil, false},
		{"开关未启用", NewMailNotifier(false, "a@qq.com", "pwd"), false},
		{"缺发件人", NewMailNotifier(true, "", "pwd"), false},
		{"缺授权码", NewMailNotifier(true, "a@qq.com", ""), false},
		{"完整配置", NewMailNotifier(true, "a@qq.com", "pwd"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.n.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMailNotifierSendDisabled(t *testing.T) {
	n := NewMailNotifier(false, "a@qq.com", "pwd")
	if err := n.Send("标题", "内容"); err != nil {
		t.Errorf("未启用时 Send 应返回 nil, 实际 %v", err)
	}
}

func TestBuildMailMessage(t *testing.T) {
	msg := buildMailMessage("test@qq.com", "标题\n注入换行", "正文第一行\n第二行")

	for _, want := range []string{
		"From: test@qq.com",
		"To: test@qq.com",
		"Content-Type: text/plain; charset=UTF-8",
		"\r\n\r\n标题\n注入换行\n\n正文第一行\n第二行",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("邮件内容缺少 %q, 实际:\n%s", want, msg)
		}
	}
	// Subject 中的换行必须压平, 且非 ASCII 走 Q 编码, 防止邮件头注入
	if strings.Contains(msg, "Subject: 标题") {
		t.Errorf("Subject 应为 Q 编码, 实际:\n%s", msg)
	}
	if !strings.Contains(msg, "Subject: =?utf-8?q?") {
		t.Errorf("Subject 应含 Q 编码前缀, 实际:\n%s", msg)
	}
	if strings.Contains(msg, "Subject: 标题\n") {
		t.Errorf("Subject 不应含裸换行, 实际:\n%s", msg)
	}
}

func TestBuildHTMLMailMessage(t *testing.T) {
	msg := buildHTMLMailMessage("test@qq.com", "盘前总结", "<html><body><h1>标题</h1></body></html>")

	for _, want := range []string{
		"From: test@qq.com",
		"To: test@qq.com",
		"Content-Type: text/html; charset=UTF-8",
		"<html><body><h1>标题</h1></body></html>",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("HTML 邮件缺少 %q, 实际:\n%s", want, msg)
		}
	}
	// Subject 换行压平 + Q 编码 (与纯文本一致, 防邮件头注入)
	if !strings.Contains(msg, "Subject: =?utf-8?q?") {
		t.Errorf("HTML 邮件 Subject 应为 Q 编码, 实际:\n%s", msg)
	}
}

func TestSendHTMLDisabled(t *testing.T) {
	n := NewMailNotifier(false, "a@qq.com", "pwd")
	if err := n.SendHTML("标题", "<b>内容</b>"); err != nil {
		t.Errorf("未启用时 SendHTML 应返回 nil, 实际 %v", err)
	}
}

func TestSendSMTP(t *testing.T) {
	srv := newFakeSMTPServer(t, "")
	msg := buildMailMessage("test@qq.com", "测试邮件", "正文内容")

	if err := sendSMTP(srv.addr(), "localhost", false, "test@qq.com", "secret", "test@qq.com", msg); err != nil {
		t.Fatalf("sendSMTP 失败: %v", err)
	}
	body := srv.received()
	for _, want := range []string{"Subject: ", "To: test@qq.com", "正文内容", "Content-Type: text/plain; charset=UTF-8"} {
		if !strings.Contains(body, want) {
			t.Errorf("SMTP DATA 内容缺少 %q, 实际:\n%s", want, body)
		}
	}
}

func TestSendSMTPAuthError(t *testing.T) {
	srv := newFakeSMTPServer(t, "AUTH")
	err := sendSMTP(srv.addr(), "localhost", false, "a@qq.com", "bad", "a@qq.com", "msg")
	if err == nil || !strings.Contains(err.Error(), "SMTP认证失败") {
		t.Fatalf("应返回 SMTP认证失败, 实际 %v", err)
	}
}

func TestSendSMTPReject(t *testing.T) {
	srv := newFakeSMTPServer(t, "MAIL")
	err := sendSMTP(srv.addr(), "localhost", false, "a@qq.com", "pwd", "a@qq.com", "msg")
	if err == nil || !strings.Contains(err.Error(), "MAIL FROM失败") {
		t.Fatalf("应返回 MAIL FROM失败, 实际 %v", err)
	}
}

// ==================== fake SMTP 服务器 ====================

// fakeSMTPServer 迷你 SMTP 服务器: 记录 DATA 阶段内容, failOn 指定返回错误的命令
type fakeSMTPServer struct {
	ln     net.Listener
	mu     sync.Mutex
	body   string
	failOn string
}

func newFakeSMTPServer(t *testing.T, failOn string) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 fake SMTP 服务器失败: %v", err)
	}
	s := &fakeSMTPServer{ln: ln, failOn: failOn}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTPServer) addr() string { return s.ln.Addr().String() }
func (s *fakeSMTPServer) received() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	reply := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}

	reply("220 fake ESMTP ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			reply("250-fake ESMTP")
			reply("250-AUTH PLAIN LOGIN")
			reply("250 OK")
		case strings.HasPrefix(upper, "AUTH"):
			if s.failOn == "AUTH" {
				reply("535 authentication failed")
				return
			}
			reply("235 ok")
		case strings.HasPrefix(upper, "MAIL"):
			if s.failOn == "MAIL" {
				reply("550 rejected")
				return
			}
			reply("250 ok")
		case strings.HasPrefix(upper, "RCPT"):
			reply("250 ok")
		case strings.HasPrefix(upper, "DATA"):
			reply("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.body = b.String()
			s.mu.Unlock()
			reply("250 queued")
		case strings.HasPrefix(upper, "QUIT"):
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}
