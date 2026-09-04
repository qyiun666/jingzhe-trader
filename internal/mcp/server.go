package mcp

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"jingzhe-trader/internal/observability"
)

// maxRequestBytes 单个 MCP 请求体上限（1MB）：调用均为小 JSON，超限即拒绝。
const maxRequestBytes = 1 << 20

// Server MCP 对外服务：/healthz 免鉴权，/mcp 需 Bearer 令牌（§10.6-2/7）。
type Server struct {
	deps      *Deps
	apiToken  string
	tools     map[string]*Tool
	mux       *http.ServeMux
	httpSrv   *http.Server
	startedAt time.Time
}

// New 构造 MCP 服务。deps 必须由组合根（internal/app.Runtime）装配完成。
// apiToken 为空时直接返回错误（拒绝无令牌启动，验收 §10.6-7）。
func New(deps Deps, apiToken string) (*Server, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("MCP 服务拒绝启动：server.api_token 为空（外部 agent 无令牌不可接入）")
	}
	s := &Server{deps: &deps, apiToken: apiToken, tools: map[string]*Tool{}, startedAt: time.Now()}
	s.registerTools()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/mcp", s.handleMCP)
	return s, nil
}

// Start 在 addr 上启动 HTTP 服务（阻塞）。除正常启动失败外，
// 主动 Shutdown 会返回 http.ErrServerClosed，调用方据此区分崩溃与关停。
func (s *Server) Start(addr string) error {
	s.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	observability.S().Infow("MCP 服务启动", "addr", addr)
	return s.httpSrv.ListenAndServe()
}

// Shutdown 停止接收新连接并等待在途请求结束。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}

// Handler 返回 HTTP 处理器（供测试使用 httptest）。
func (s *Server) Handler() http.Handler { return s.mux }

// healthTickStale 超过该时长没有调度判定即视为调度停摆（默认 tick 30s，留 4 倍余量）。
const healthTickStale = 2 * time.Minute

// handleHealthz 报告进程与调度循环的真实状态。
// 调度停摆返回 503，便于外部 agent 用 `curl -f` 一类方式判活并重启。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	out := map[string]interface{}{
		"status":   "ok",
		"service":  "jingzhe",
		"time":     now.UTC().Format(time.RFC3339),
		"uptime_s": int64(now.Sub(s.startedAt).Seconds()),
	}
	code := http.StatusOK
	if lv := s.deps.Liveness; lv != nil {
		running := lv.IsRunning()
		last := lv.LastTickAt()
		out["scheduler_running"] = running
		if last.IsZero() {
			out["last_tick_ago_s"] = int64(-1)
		} else {
			out["last_tick_ago_s"] = int64(now.Sub(last).Seconds())
		}
		if !running || now.Sub(last) > healthTickStale {
			out["status"] = "unhealthy"
			code = http.StatusServiceUnavailable
		}
	}
	writeJSON(w, code, out)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 鉴权：所有 /mcp 请求需 Bearer 令牌。
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !hmac.Equal([]byte(tok), []byte(s.apiToken)) {
		writeJSON(w, 401, map[string]interface{}{
			"jsonrpc": "2.0",
			"error":   map[string]interface{}{"code": 401, "message": "未授权：缺少或错误的 Bearer 令牌"},
		})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, 400, rpcError(0, -32700, "请求体读取失败: "+err.Error()))
		return
	}
	var req JSONRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, rpcError(0, -32700, "JSON 解析失败: "+err.Error()))
		return
	}
	s.dispatch(w, r.Context(), &req)
}

func (s *Server) dispatch(w http.ResponseWriter, ctx context.Context, req *JSONRPCRequest) {
	switch req.Method {
	case "initialize":
		writeJSON(w, 200, JSONRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
				"serverInfo":      map[string]interface{}{"name": "jingzhe-trader", "version": "0.1.0"},
			},
		})
	case "tools/list":
		tools := make([]map[string]interface{}, 0, len(s.tools))
		for _, t := range s.tools {
			tools = append(tools, map[string]interface{}{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		writeJSON(w, 200, JSONRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]interface{}{"tools": tools},
		})
	case "tools/call":
		var p struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}
		if err := json.Unmarshal(req.RawParams, &p); err != nil {
			writeJSON(w, 200, rpcError(req.ID, -32602, "参数解析失败: "+err.Error()))
			return
		}
		t, ok := s.tools[p.Name]
		if !ok {
			observability.S().Errorw("MCP 调用被拒：未知工具", "tool", p.Name, "args", clipArgs(p.Arguments))
			writeJSON(w, 200, rpcError(req.ID, -32601, "未知工具: "+p.Name))
			return
		}
		if err := checkRequiredArgs(t, p.Arguments); err != nil {
			observability.S().Errorw("MCP 调用被拒：必填参数缺失",
				"tool", p.Name, "args", clipArgs(p.Arguments), "err", err.Error())
			writeJSON(w, 200, toolError(req.ID, err))
			return
		}
		if err := checkDateArgs(t, p.Arguments); err != nil {
			observability.S().Errorw("MCP 调用被拒：日期格式非法",
				"tool", p.Name, "args", clipArgs(p.Arguments), "err", err.Error())
			writeJSON(w, 200, toolError(req.ID, err))
			return
		}
		// 外部 agent 的每一次动作都要留痕：它不经过调度器，runJob 那层日志覆盖不到它，
		// 没有这一行就只能靠"结果表变了没有"反推 agent 到底动过什么。
		started := time.Now()
		res, err := t.Handler(ctx, p.Arguments)
		if err != nil {
			observability.S().Errorw("MCP 工具执行失败", "tool", p.Name, "args", clipArgs(p.Arguments),
				"secs", time.Since(started).Seconds(), "err", err.Error())
			writeJSON(w, 200, toolError(req.ID, err))
			return
		}
		observability.S().Infow("MCP 工具执行完成", "tool", p.Name, "args", clipArgs(p.Arguments),
			"secs", time.Since(started).Seconds())
		text, _ := json.MarshalIndent(res, "", "  ")
		writeJSON(w, 200, JSONRPCResponse{
			JSONRPC: "2.0", ID: req.ID,
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": string(text)},
				},
				"isError": false,
			},
		})
	default:
		writeJSON(w, 200, rpcError(req.ID, -32601, "不支持的方法: "+req.Method))
	}
}

// ---- JSON-RPC 基础结构 ----

type JSONRPCRequest struct {
	JSONRPC   string          `json:"jsonrpc"`
	ID        interface{}     `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	RawParams json.RawMessage `json:"-"`
}

type JSONRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
}

func (r *JSONRPCRequest) UnmarshalJSON(b []byte) error {
	type alias JSONRPCRequest
	aux := alias{}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*r = JSONRPCRequest(aux)
	r.RawParams = aux.Params
	return nil
}

func rpcError(id interface{}, code int, msg string) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: "2.0", ID: id,
		Error: map[string]interface{}{"code": code, "message": msg},
	}
}

// toolError 工具调用失败的返回：按 MCP 约定放在 result.content 里并标 isError，
// 而不是协议级 error（客户端据此判断"这次调用失败了"，但仍能读到原因）。
// clipArgs 工具入参的单行摘要（审计用）。参数里没有凭据，但 reason/note 是长自由文本，
// 整包打出来会把日志刷满，所以按 JSON 序列化后截断。
func clipArgs(args map[string]interface{}) string {
	raw, err := json.Marshal(args)
	if err != nil {
		return fmt.Sprintf("<参数无法序列化: %v>", err)
	}
	r := string(raw)
	if len(r) > 240 {
		return r[:240] + "…"
	}
	return r
}

func toolError(id interface{}, cause error) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: "2.0", ID: id,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": "错误: " + cause.Error()},
			},
			"isError": true,
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
