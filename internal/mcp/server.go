package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// Server MCP 对外服务：/healthz 免鉴权，/mcp 需 Bearer 令牌（§10.6-2/7）。
type Server struct {
	deps     *Deps
	apiToken string
	tools    map[string]*Tool
	mux      *http.ServeMux
}

// New 构造 MCP 服务。apiToken 为空时直接返回错误（拒绝无令牌启动，验收 §10.6-7）。
func New(st *store.Store, cfg *config.Config, apiToken string) (*Server, error) {
	if apiToken == "" {
		return nil, fmt.Errorf("MCP 服务拒绝启动：server.api_token 为空（外部 agent 无令牌不可接入）")
	}
	ctx := context.Background()
	deps, err := BuildDeps(ctx, st, cfg)
	if err != nil {
		return nil, fmt.Errorf("装配 MCP 依赖失败: %w", err)
	}
	s := &Server{deps: deps, apiToken: apiToken, tools: map[string]*Tool{}}
	s.registerTools()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/mcp", s.handleMCP)
	return s, nil
}

// Start 在 addr 上启动 HTTP 服务（阻塞）。
func (s *Server) Start(addr string) error {
	observability.S().Infow("MCP 服务启动", "addr", addr)
	return http.ListenAndServe(addr, s.mux)
}

// Handler 返回 HTTP 处理器（供测试使用 httptest）。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
		"service": "jingzhe-trader-mcp",
	})
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 鉴权：所有 /mcp 请求需 Bearer 令牌。
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if !constantTimeEqual(tok, s.apiToken) {
		writeJSON(w, 401, map[string]interface{}{
			"jsonrpc": "2.0",
			"error":   map[string]interface{}{"code": 401, "message": "未授权：缺少或错误的 Bearer 令牌"},
		})
		return
	}
	body, err := io.ReadAll(r.Body)
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
			writeJSON(w, 200, rpcError(req.ID, -32601, "未知工具: "+p.Name))
			return
		}
		res, err := t.Handler(ctx, p.Arguments)
		if err != nil {
			writeJSON(w, 200, JSONRPCResponse{
				JSONRPC: "2.0", ID: req.ID,
				Result: map[string]interface{}{
					"content": []map[string]interface{}{
						{"type": "text", "text": "错误: " + err.Error()},
					},
					"isError": true,
				},
			})
			return
		}
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
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// constantTimeEqual 防时序侧信道的字符串比较。
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
