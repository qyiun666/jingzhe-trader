package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fastBackoff 缩短重试退避间隔, 加速测试
func fastBackoff(t *testing.T) {
	t.Helper()
	old := retryBackoffBase
	retryBackoffBase = time.Millisecond
	t.Cleanup(func() { retryBackoffBase = old })
}

func newTestClient(baseURL string) *Client {
	return NewClient(Config{APIKey: "test-key", BaseURL: baseURL, Model: "test-model", Enabled: true})
}

// writeOKResponse 返回合法的 OpenAI 兼容响应
func writeOKResponse(w http.ResponseWriter, content string) {
	json.NewEncoder(w).Encode(map[string]any{
		"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": content}}},
	})
}

// statusSequenceServer 按序返回给定状态码, 用完后返回 200 合法响应
func statusSequenceServer(codes ...int) (*httptest.Server, *int32) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if int(n) <= len(codes) {
			w.WriteHeader(codes[n-1])
			w.Write([]byte(`{"error":{"message":"mock error"}}`))
			return
		}
		writeOKResponse(w, `{"sentiment":0.5}`)
	}))
	return srv, &calls
}

// TestChatRetryOn5xx 5xx 服务端错误应重试并最终成功
func TestChatRetryOn5xx(t *testing.T) {
	fastBackoff(t)
	srv, calls := statusSequenceServer(http.StatusInternalServerError)
	defer srv.Close()

	content, err := newTestClient(srv.URL).Chat("sys", "user")
	if err != nil {
		t.Fatalf("5xx 后重试应成功, 实际错误: %v", err)
	}
	if content == "" {
		t.Errorf("响应内容不应为空")
	}
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Errorf("5xx 应重试, 期望请求 2 次, 实际 %d 次", got)
	}
}

// TestChatRetryOn429 429 限流应重试并最终成功
func TestChatRetryOn429(t *testing.T) {
	fastBackoff(t)
	srv, calls := statusSequenceServer(http.StatusTooManyRequests, http.StatusServiceUnavailable)
	defer srv.Close()

	if _, err := newTestClient(srv.URL).Chat("sys", "user"); err != nil {
		t.Fatalf("429/503 后重试应成功, 实际错误: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("期望请求 3 次(429+503+成功), 实际 %d 次", got)
	}
}

// TestChatNoRetryOn4xx 4xx 业务错误不应重试
func TestChatNoRetryOn4xx(t *testing.T) {
	fastBackoff(t)
	srv, calls := statusSequenceServer(http.StatusBadRequest, http.StatusBadRequest, http.StatusBadRequest)
	defer srv.Close()

	_, err := newTestClient(srv.URL).Chat("sys", "user")
	if err == nil {
		t.Fatalf("持续 400 应返回错误")
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("4xx 不应重试, 期望请求 1 次, 实际 %d 次", got)
	}
}

// TestChatRetryExhausted 持续 5xx 时最多尝试 maxAttempts 次后报错
func TestChatRetryExhausted(t *testing.T) {
	fastBackoff(t)
	srv, calls := statusSequenceServer(
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusInternalServerError)
	defer srv.Close()

	_, err := newTestClient(srv.URL).Chat("sys", "user")
	if err == nil {
		t.Fatalf("持续 5xx 应返回错误")
	}
	if got := atomic.LoadInt32(calls); got != maxAttempts {
		t.Errorf("期望最多请求 %d 次, 实际 %d 次", maxAttempts, got)
	}
}

// TestChatNetworkErrorRetry 网络层错误(连接被拒)应重试后报错
func TestChatNetworkErrorRetry(t *testing.T) {
	fastBackoff(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭, 后续请求必然连接失败

	if _, err := newTestClient(url).Chat("sys", "user"); err == nil {
		t.Fatalf("连接失败应返回错误")
	}
}

// TestChatMaxTokens 请求体应使用提升后的 max_tokens=2048
func TestChatMaxTokens(t *testing.T) {
	var gotMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotMaxTokens = req.MaxTokens
		writeOKResponse(w, "ok")
	}))
	defer srv.Close()

	if _, err := newTestClient(srv.URL).Chat("sys", "user"); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if gotMaxTokens != 2048 {
		t.Errorf("max_tokens 应为 2048, 实际 %d", gotMaxTokens)
	}
}

// TestChatJSONMode 显式开启 json_mode 时请求体应携带 response_format=json_object
func TestChatJSONMode(t *testing.T) {
	var gotFormat *ResponseFormat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotFormat = req.ResponseFormat
		writeOKResponse(w, "ok")
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "test-model", Enabled: true, JSONMode: boolPtr(true)})
	if _, err := client.Chat("sys", "user"); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if gotFormat == nil || gotFormat.Type != "json_object" {
		t.Errorf("json_mode 开启时 response_format 应为 json_object, 实际 %+v", gotFormat)
	}
}

// TestChatJSONModeDisabled 关闭 json_mode 时请求体不携带 response_format
func TestChatJSONModeDisabled(t *testing.T) {
	var gotFormat *ResponseFormat
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatCompletionRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotFormat = req.ResponseFormat
		writeOKResponse(w, "ok")
	}))
	defer srv.Close()

	client := NewClient(Config{APIKey: "test-key", BaseURL: srv.URL, Model: "test-model", Enabled: true, JSONMode: boolPtr(false)})
	if _, err := client.Chat("sys", "user"); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if gotFormat != nil {
		t.Errorf("json_mode 关闭时不应携带 response_format, 实际 %+v", gotFormat)
	}
}

// boolPtr 构造 bool 指针
func boolPtr(v bool) *bool { return &v }

// TestChatWithCache 同日同股票同角色同输入应命中缓存, 不重复调用 LLM
func TestChatWithCache(t *testing.T) {
	fastBackoff(t)
	srv, calls := statusSequenceServer()
	defer srv.Close()
	client := newTestClient(srv.URL)

	// 相同 key 调用两次, 只应请求一次
	for i := 0; i < 2; i++ {
		if _, err := client.ChatWithCache("20260102", "600519.SH", "technical", "sys", "user"); err != nil {
			t.Fatalf("第%d次调用失败: %v", i+1, err)
		}
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("相同 key 应命中缓存, 期望请求 1 次, 实际 %d 次", got)
	}

	// 角色不同, key 不同, 应再次请求
	if _, err := client.ChatWithCache("20260102", "600519.SH", "fundamental", "sys", "user"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// 日期不同, key 不同, 应再次请求
	if _, err := client.ChatWithCache("20260103", "600519.SH", "technical", "sys", "user"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != 3 {
		t.Errorf("不同角色/日期不应命中缓存, 期望请求 3 次, 实际 %d 次", got)
	}
}

// TestChatWithCacheNotCacheError 失败响应不应写入缓存
func TestChatWithCacheNotCacheError(t *testing.T) {
	fastBackoff(t)
	// 前 maxAttempts 次全部 500 (重试耗尽), 之后恢复 200
	srv, calls := statusSequenceServer(
		http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError)
	defer srv.Close()
	client := newTestClient(srv.URL)

	if _, err := client.ChatWithCache("20260102", "600519.SH", "news", "sys", "user"); err == nil {
		t.Fatalf("持续 5xx 应返回错误")
	}
	if _, err := client.ChatWithCache("20260102", "600519.SH", "news", "sys", "user"); err != nil {
		t.Fatalf("错误不应被缓存, 恢复后应成功: %v", err)
	}
	if got := atomic.LoadInt32(calls); got != maxAttempts+1 {
		t.Errorf("期望请求 %d 次, 实际 %d 次", maxAttempts+1, got)
	}
}

// TestChatDisabled 未启用客户端直接报错
func TestChatDisabled(t *testing.T) {
	client := NewClient(Config{})
	if client.IsEnabled() {
		t.Fatalf("无 API Key 应为禁用状态")
	}
	if _, err := client.Chat("sys", "user"); err == nil {
		t.Errorf("禁用客户端 Chat 应返回错误")
	}
	if _, err := client.ChatWithCache("20260102", "600519.SH", "technical", "sys", "user"); err == nil {
		t.Errorf("禁用客户端 ChatWithCache 应返回错误")
	}
}
