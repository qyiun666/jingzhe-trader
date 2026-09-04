package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

func newTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	// server.api_token / tushare.token 为必配项（无默认值），由环境变量应急覆盖通道供给，使 config.Load 自检通过。
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	t.Setenv("JZ_SERVER_API_TOKEN", "testtoken")

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	// MCP 只消费注入的实例；这里按被测工具所需最小集装配。
	deps := Deps{
		Store:     st,
		Config:    cfg,
		Goal:      goal.NewService(st, goal.DefaultConfig(), ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000))),
		Freshness: dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), 0),
		Ledger:    ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000)),
		Tickets:   ticket.NewService(st),
	}
	srv, err := New(deps, "testtoken")
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

	// 4. tools/list：对外工具名全集就是契约，改名或漏注册必须在此显式确认
	list := mcpCall(t, ts, "testtoken", "tools/list", nil)
	res, _ := list["result"].(map[string]interface{})
	tools, _ := res["tools"].([]interface{})
	got := make(map[string]bool, len(tools))
	for _, v := range tools {
		m, _ := v.(map[string]interface{})
		name, _ := m["name"].(string)
		got[name] = true
	}
	want := []string{
		"get_brief", "get_tickets", "get_positions", "get_portfolio", "get_logs",
		"init_day", "report_fill", "sync_portfolio", "skip_ticket", "set_gear", "confirm_pace", "trigger_task",
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("tools/list 缺少工具 %s", n)
		}
		delete(got, n)
	}
	for extra := range got {
		t.Errorf("tools/list 出现未登记在册的工具 %s", extra)
	}

	// 5. get_brief 调用
	if b := mcpCallOK(t, ts, "get_brief", map[string]interface{}{"date": "20260901"}); b["trade_date"] != "20260901" {
		t.Fatalf("get_brief 返回异常: %v", b)
	}

	// 6. get_logs 调用
	mcpCallOK(t, ts, "get_logs", map[string]interface{}{"date": "20260901"})

	// 7. 非法指令单状态必须报错：静默返回空列表会让 agent 误判"今日无指令"
	mcpCallErr(t, ts, "get_tickets", map[string]interface{}{"date": "20260901", "status": "rejected"})

	// 8. sync_portfolio：首次写入本金，二次拒绝覆盖（验收 #14）
	positions := []interface{}{
		map[string]interface{}{"ts_code": "600519.SH", "total_qty": 100, "today_bought": 100, "cost_price": 1500.5},
		map[string]interface{}{"ts_code": "000001.SZ", "total_qty": 1000, "cost_price": 11.2, "high_price": 12.8},
	}
	first := mcpCallOK(t, ts, "sync_portfolio", map[string]interface{}{
		"date": "20260901", "initial_capital_yuan": 10000, "available_cash_yuan": 9000, "positions": positions,
	})
	if first["synced"] != float64(2) {
		t.Fatalf("sync_portfolio 首次应同步 2 只持仓: %v", first)
	}
	if first["capital_rejected"] == true {
		t.Fatalf("本金首次写入不应被拒: %v", first)
	}
	// 可用资金必须取券商给的锚点，不能再把持仓成本算成现金
	if got, _ := first["cash_after_sync"].(string); !strings.Contains(got, "9,000") {
		t.Fatalf("同步后可用资金应回显券商给的 9,000 元，实际 %q", got)
	}
	mcpCallErr(t, ts, "sync_portfolio", map[string]interface{}{
		"date": "20260902", "initial_capital_yuan": 20000, "positions": positions,
	})
	second := mcpCallOK(t, ts, "sync_portfolio", map[string]interface{}{
		"date": "20260902", "initial_capital_yuan": 20000, "available_cash_yuan": 9000, "positions": positions,
	})
	if second["capital_rejected"] != true {
		t.Fatalf("第二次同步本金应被拒绝（write-once）: %v", second)
	}
	if n, _ := second["synced"].(float64); n != 2 {
		t.Fatalf("本金被拒时持仓仍应照常同步: %v", second)
	}
	t.Logf("MCP 冒烟通过：%d 个工具在册，healthz/鉴权/initialize/读工具/写工具幂等语义均正常", len(want))
}

// mcpCallOK 调 tools/call 并解出 content[0].text 内的 JSON；出错即失败。
func mcpCallOK(t *testing.T, ts *httptest.Server, name string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{"name": name, "arguments": args})
	out, _ := res["result"].(map[string]interface{})
	if out == nil {
		t.Fatalf("%s 无 result: %v", name, res)
	}
	if out["isError"] == true {
		t.Fatalf("%s 期望成功，实际报错: %v", name, out["content"])
	}
	content, _ := out["content"].([]interface{})
	if len(content) == 0 {
		t.Fatalf("%s 无返回内容", name)
	}
	text, _ := content[0].(map[string]interface{})["text"].(string)
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("%s 返回非 JSON 对象: %v (%s)", name, err, text)
	}
	return payload
}

// mcpCallErr 断言工具以业务错误拒绝入参，而不是静默返回空结果。
func mcpCallErr(t *testing.T, ts *httptest.Server, name string, args map[string]interface{}) {
	t.Helper()
	res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{"name": name, "arguments": args})
	out, _ := res["result"].(map[string]interface{})
	if out == nil || out["isError"] != true {
		t.Fatalf("%s(%v) 期望业务报错，实际: %v", name, args, res)
	}
}

// fakeLiveness 可控的调度器探活桩。
type fakeLiveness struct {
	running bool
	last    time.Time
}

func (f fakeLiveness) IsRunning() bool       { return f.running }
func (f fakeLiveness) LastTickAt() time.Time { return f.last }

// TestHealthzReflectsSchedulerState 探活必须区分「接口活着」与「调度在跑」：
// 主循环退出或久无判定时回落 503，agent 才判得出需要重启。
func TestHealthzReflectsSchedulerState(t *testing.T) {
	cases := []struct {
		name string
		lv   Liveness
		want int
	}{
		{"未注入调度器_仅接口层存活", nil, http.StatusOK},
		{"在跑且刚完成判定", fakeLiveness{running: true, last: time.Now()}, http.StatusOK},
		{"主循环已退出", fakeLiveness{running: false, last: time.Now()}, http.StatusServiceUnavailable},
		{"声称在跑但久无判定", fakeLiveness{running: true, last: time.Now().Add(-3 * time.Minute)}, http.StatusServiceUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, err := New(Deps{Liveness: c.lv}, "tok")
			if err != nil {
				t.Fatalf("构造失败: %v", err)
			}
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if rr.Code != c.want {
				t.Fatalf("状态码期望 %d 实际 %d，body=%s", c.want, rr.Code, rr.Body.String())
			}
		})
	}
}
