package notify

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"
)

// TestSMTPDeadlineStopsHungSession 对端完成 TLS 握手后不再应答，
// Send 必须在 smtpIOTimeout 内返回错误，而不是把同步的调度 Tick 挂死。
func TestSMTPDeadlineStopsHungSession(t *testing.T) {
	cert := selfSignedCert(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("监听测试 TLS 端口失败: %v", err)
	}
	defer ln.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if tc, ok := conn.(*tls.Conn); ok {
			_ = tc.Handshake()
		}
		<-stop // 握手后故意不应答，模拟 SMTP 挂死
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("解析监听地址失败: %v", err)
	}
	port, _ := strconv.Atoi(portStr)

	oldTimeout, oldTLS := smtpIOTimeout, smtpTLSConfig
	smtpIOTimeout = 300 * time.Millisecond
	smtpTLSConfig = func(host string) *tls.Config {
		return &tls.Config{ServerName: host, InsecureSkipVerify: true}
	}
	defer func() {
		smtpIOTimeout = oldTimeout
		smtpTLSConfig = oldTLS
	}()

	cfg := SMTPConfig{Host: host, Port: port, From: "from@example.com", Password: "x"}
	start := time.Now()
	err = cfg.Send([]string{"to@example.com"}, "subject", "body")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("对端挂死时 Send 应返回错误")
	}
	if elapsed > 3*time.Second {
		t.Errorf("Send 耗时 %s，smtpIOTimeout=%s 未生效", elapsed, smtpIOTimeout)
	}
}

// selfSignedCert 生成本机 127.0.0.1 的自签服务端证书（测试专用）。
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("生成测试私钥失败: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("生成测试证书失败: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
