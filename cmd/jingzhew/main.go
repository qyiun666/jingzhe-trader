// jingzhew：惊蛰看门狗（Watchdog）。
//
// 独立二进制，刻意不 import 任何 internal 业务包；仅用标准库轮询 jingzhed 健康端点，
// 连续失败达到阈值时通过 SMTP 发送邮件告警（同样通过标准库 net/smtp 实现）。
//
// 用法:
//   jingzhew -addr http://127.0.0.1:8080 -interval 60s [-once]
//
// 告警投递依赖以下环境变量（或对应 flag）：
//   JZ_WATCHDOG_SMTP_HOST / PORT / USER / PASS / MAIL_TO
//   JZ_WATCHDOG_TOKEN（可选，用于探测 /mcp 鉴权链路）
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", envOr("JZ_WATCHDOG_ADDR", "http://127.0.0.1:8080"), "jingzhed 基址")
	interval := flag.Duration("interval", envDur("JZ_WATCHDOG_INTERVAL", 60*time.Second), "轮询间隔")
	once := flag.Bool("once", false, "只检测一次并退出（冒烟/调试用）")
	token := flag.String("token", os.Getenv("JZ_WATCHDOG_TOKEN"), "MCP Bearer 令牌（可选，探测 /mcp 用）")
	smtpHost := flag.String("smtp-host", os.Getenv("JZ_WATCHDOG_SMTP_HOST"), "告警 SMTP 主机")
	smtpPort := flag.String("smtp-port", envOr("JZ_WATCHDOG_SMTP_PORT", "465"), "告警 SMTP 端口")
	smtpUser := flag.String("smtp-user", os.Getenv("JZ_WATCHDOG_SMTP_USER"), "告警 SMTP 用户")
	smtpPass := flag.String("smtp-pass", os.Getenv("JZ_WATCHDOG_SMTP_PASS"), "告警 SMTP 密码")
	mailTo := flag.String("mail-to", os.Getenv("JZ_WATCHDOG_MAIL_TO"), "告警收件人")
	failThreshold := flag.Int("fail-threshold", 3, "连续失败多少次才发告警")
	flag.Parse()

	client := &http.Client{Timeout: 10 * time.Second}
	var consec int
	for {
		err := probe(client, *addr, *token)
		if err != nil {
			consec++
			logf("探测失败 (%d/%d): %v", consec, *failThreshold, err)
			if consec >= *failThreshold {
				if *smtpHost != "" && *mailTo != "" {
					if e := sendAlert(*smtpHost, *smtpPort, *smtpUser, *smtpPass, *mailTo, *addr, err); e != nil {
						logf("告警发送失败: %v", e)
					} else {
						logf("已发送告警邮件 -> %s", *mailTo)
					}
				}
				consec = 0 // 重置，避免重复轰炸
			}
		} else {
			if consec > 0 {
				logf("jingzhed 已恢复")
			}
			consec = 0
		}
		if *once {
			if err != nil {
				os.Exit(1)
			}
			return
		}
		time.Sleep(*interval)
	}
}

// probe 探测 /healthz（免鉴权）与 /mcp initialize（鉴权链路）。两者任一失败即判为不健康。
func probe(client *http.Client, addr, token string) error {
	base := strings.TrimRight(addr, "/")
	// 1) healthz
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		return fmt.Errorf("healthz 请求失败: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz 返回 %d: %s", resp.StatusCode, string(body))
	}
	var hz struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &hz); err != nil || hz.Status != "ok" {
		return fmt.Errorf("healthz 响应异常: %s", string(body))
	}

	// 2) /mcp initialize（若提供了 token，验证鉴权链路；否则仅验证端点可达）
	if token == "" {
		return nil
	}
	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]interface{}{},
	}
	pb, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(string(pb)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	mresp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp 请求失败: %w", err)
	}
	mbody, _ := io.ReadAll(mresp.Body)
	mresp.Body.Close()
	if mresp.StatusCode != 200 {
		return fmt.Errorf("mcp 返回 %d: %s", mresp.StatusCode, string(mbody))
	}
	return nil
}

// sendAlert 通过 SMTP 发送告警邮件（标准库实现，支持 465 隐式 TLS 与 587 STARTTLS）。
func sendAlert(host, port, user, pass, to, addr string, probeErr error) error {
	subject := "【惊蛰看门狗】jingzhed 不可达"
	msg := fmt.Sprintf("Subject: %s\r\nTo: %s\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\njingzhed 探测连续失败。\n地址: %s\n错误: %v\n时间: %s\n",
		subject, to, addr, probeErr, time.Now().Format(time.RFC3339))

	var c *smtp.Client
	var err error
	if port == "465" {
		conn, derr := tls.Dial("tcp", net.JoinHostPort(host, port), &tls.Config{ServerName: host})
		if derr != nil {
			return derr
		}
		c, err = smtp.NewClient(conn, host)
	} else {
		c, err = smtp.Dial(net.JoinHostPort(host, port))
	}
	if err != nil {
		return err
	}
	defer c.Quit()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if user != "" {
		if err := c.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
			return err
		}
	}
	if err := c.Mail(user); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	_, err = w.Write([]byte(msg))
	if err != nil {
		return err
	}
	return w.Close()
}

func logf(format string, args ...interface{}) {
	fmt.Printf("[jingzhew %s] %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
