package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

func newTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	_ = os.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	// server.api_token 为必配项（无默认值），由环境变量应急覆盖通道供给，使 config.Load 自检通过。
	_ = os.Setenv("JZ_SERVER_API_TOKEN", "testtoken")
	st, err := store.Open("/Users/zt_mac/zt_hd/projects/jingzhe-trader/data/jingzhe.db")
	if err != nil {
		t.Skipf("打开验证库失败（非致命）: %v", err)
	}
	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	srv, err := New(st, cfg, "testtoken")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	return ts, func() { ts.Close(); st.Close() }
}

func mcpCall(t *testing.T, ts *httptest.Server, token, method string, params interface{}) map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应解析失败 %s: %v", raw, err)
	}
	return out
}

func TestMCPSmoke(t *testing.T) {
	ts, cleanup := newTestServer(t)
	defer cleanup()

	// 1. healthz 免鉴权
	r, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 {
		t.Fatalf("healthz 期望 200，实际 %d", r.StatusCode)
	}

	// 2. 无令牌 -> 401
	noTok := mcpCall(t, ts, "", "initialize", nil)
	if _, ok := noTok["error"]; !ok {
		t.Fatalf("无令牌应返回 error，实际 %v", noTok)
	}

	// 3. initialize
	init := mcpCall(t, ts, "testtoken", "initialize", nil)
	if init["error"] != nil {
		t.Fatalf("initialize 失败: %v", init["error"])
	}

	// 4. tools/list 13 个工具
	list := mcpCall(t, ts, "testtoken", "tools/list", nil)
	res, _ := list["result"].(map[string]interface{})
	tools, _ := res["tools"].([]interface{})
	if len(tools) != 13 {
		t.Fatalf("期望 13 个工具，实际 %d: %v", len(tools), res)
	}

	// 5. get_brief 调用
	brief := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{
		"name":      "get_brief",
		"arguments": map[string]interface{}{"date": "20260901"},
	})
	bres, _ := brief["result"].(map[string]interface{})
	if bres == nil || bres["isError"] == true {
		t.Fatalf("get_brief 失败: %v", brief)
	}

	// 6. get_logs 调用
	logs := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{
		"name":      "get_logs",
		"arguments": map[string]interface{}{"date": "20260901"},
	})
	lres, _ := logs["result"].(map[string]interface{})
	if lres == nil || lres["isError"] == true {
		t.Fatalf("get_logs 失败: %v", logs)
	}
	t.Logf("MCP 冒烟测试通过：healthz/鉴权/initialize/tools.list(13)/get_brief/get_logs 均正常")
}
