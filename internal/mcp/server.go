// Package mcp exposes the Jingzhe trading system capabilities through the
// Model Context Protocol (MCP). It reuses the business logic from
// internal/api by calling the exported Service methods.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"jingzhe-trader/internal/api"
	"jingzhe-trader/internal/config"
)

// Server wraps an MCP server on top of the existing api.Service.
type Server struct {
	svc *api.Service
	cfg *config.Config
	mcp *mcpserver.MCPServer
}

// NewServer creates and configures the MCP server.
func NewServer(svc *api.Service, cfg *config.Config) *Server {
	s := &Server{svc: svc, cfg: cfg}
	s.mcp = mcpserver.NewMCPServer(
		"jingzhe-trader",
		"1.0.0",
		mcpserver.WithToolCapabilities(true),
	)
	s.registerTools()
	s.registerResources()
	return s
}

// Handler returns an http.Handler that serves MCP over Streamable HTTP.
// This keeps the server as a single resident background process while
// exposing only the MCP interface externally.
func (s *Server) Handler() http.Handler {
	return mcpserver.NewStreamableHTTPServer(
		s.mcp,
		mcpserver.WithEndpointPath("/mcp"),
		mcpserver.WithDisableLocalhostProtection(true),
	)
}

// ServeStdio runs the MCP server over stdio, useful for local CLI clients.
func (s *Server) ServeStdio() error {
	return mcpserver.ServeStdio(s.mcp)
}

func (s *Server) registerTools() {
	// ---------- read tools ----------

	s.addTool("get_health",
		"Get server health status (uptime, goroutines, db size, data freshness, job health).",
		s.handleGetHealth)

	s.addTool("get_agent_brief",
		"Get the full agent context: plans, portfolio, market, debates, decision changes, task status, and goal.",
		s.handleGetAgentBrief)

	s.addTool("get_agent_dashboard",
		"Get the agent dashboard summary: unread alerts, today's alerts, open plans, debates, decision changes, and task status.",
		s.handleGetAgentDashboard)

	s.addTool("get_agent_changes",
		"Get decision changes, plan status changes, and task status for a date.",
		s.handleGetAgentChanges,
		mcp.WithString("date", mcp.Description("Date in YYYYMMDD format; defaults to latest trade date")))

	s.addTool("get_agent_alerts",
		"Get recent or unread agent alerts (notifications persisted to the alert store).",
		s.handleGetAgentAlerts,
		mcp.WithBoolean("unread_only", mcp.Description("Only return unread alerts"), mcp.DefaultBool(false)),
		mcp.WithString("date", mcp.Description("Optional date filter in YYYYMMDD format")))

	s.addTool("get_positions",
		"Get the current portfolio diagnosis (positions, asset, health score, risk metrics).",
		s.handleGetPositions,
		mcp.WithString("date", mcp.Description("Date in YYYYMMDD format; defaults to today")))

	s.addTool("get_portfolio",
		"Get the raw portfolio holdings from the database.",
		s.handleGetPortfolio)

	s.addTool("get_market",
		"Get the market snapshot for a date.",
		s.handleGetMarket,
		mcp.WithString("date", mcp.Description("Date in YYYYMMDD format; defaults to today")))

	s.addTool("get_daily_report",
		"Get the daily trading report for a date and strategy.",
		s.handleGetDailyReport,
		mcp.WithString("date", mcp.Description("Date in YYYYMMDD format; defaults to today")),
		mcp.WithString("strategy", mcp.Description("Strategy name; defaults to dynamic strategy selection")))

	s.addTool("get_plans",
		"Get trade plan list for a date, or all open plans when date is omitted.",
		s.handleGetPlans,
		mcp.WithString("date", mcp.Description("Optional date in YYYYMMDD format")))

	s.addTool("get_strategy_status",
		"Get the dynamic strategy selector status (current strategy, market condition, confidence, recommendation).",
		s.handleGetStrategyStatus)

	s.addTool("get_goal_status",
		"Get the quarterly goal tracking status (return, drawdown, budget consumed, risk mode).",
		s.handleGetGoalStatus,
		mcp.WithString("date", mcp.Description("Date in YYYYMMDD format; defaults to today")))

	s.addTool("get_screener_results",
		"Get the latest or historical automatic stock screening results.",
		s.handleGetScreenerResults,
		mcp.WithString("date", mcp.Description("Optional date in YYYYMMDD format")))

	s.addTool("get_system_status",
		"Get overall system status (data freshness, portfolio count, next market open, uptime).",
		s.handleGetSystemStatus)

	s.addTool("get_kline",
		"Get K-line bars for a stock.",
		s.handleGetKline,
		mcp.WithString("code", mcp.Required(), mcp.Description("Stock code, e.g. 510050.SH")),
		mcp.WithString("start", mcp.Description("Start date in YYYYMMDD format; defaults to 20200101")),
		mcp.WithString("end", mcp.Description("End date in YYYYMMDD format; defaults to today")))

	s.addTool("get_snapshots",
		"Get historical account snapshots.",
		s.handleGetSnapshots,
		mcp.WithNumber("limit", mcp.Description("Maximum number of snapshots to return"), mcp.DefaultNumber(30)))

	// ---------- write tools ----------

	s.addTool("confirm_plan",
		"Confirm a pending trade plan by ID. In QMT+auto_execute mode this places a real order.",
		s.handleConfirmPlan,
		mcp.WithNumber("id", mcp.Required(), mcp.Description("Trade plan ID")))

	s.addTool("confirm_trade",
		"Report a manual trade execution so the local portfolio stays in sync.",
		s.handleConfirmTrade,
		mcp.WithString("ts_code", mcp.Required(), mcp.Description("Stock code, e.g. 000001.SZ")),
		mcp.WithString("side", mcp.Required(), mcp.Description("buy or sell")),
		mcp.WithNumber("qty", mcp.Required(), mcp.Description("Trade quantity (must be a multiple of 100)")),
		mcp.WithNumber("price", mcp.Required(), mcp.Description("Trade price")),
		mcp.WithNumber("plan_id", mcp.Description("Optional trade plan ID to close after confirmation"),
			mcp.DefaultNumber(0)))

	s.addTool("sync_portfolio",
		"Synchronize the local portfolio with real broker positions.",
		s.handleSyncPortfolio,
		mcp.WithNumber("cash", mcp.Description("Available cash; omit to keep current cash")),
		mcp.WithBoolean("overwrite", mcp.Description("If true, replace all positions; if false, upsert incrementally"), mcp.DefaultBool(true)),
		mcp.WithArray("positions", mcp.Required(), mcp.Description("Array of position objects {ts_code, total_qty, available_qty, cost_price}")))

	s.addTool("update_data",
		"Manually trigger an incremental data update (Tushare bars, calendar, basics).",
		s.handleUpdateData)

	s.addTool("switch_strategy",
		"Manually switch the active strategy used for signal generation.",
		s.handleSwitchStrategy,
		mcp.WithString("strategy", mcp.Required(), mcp.Description("Strategy name, e.g. ma_cross, macd, multi_factor")))

	s.addTool("run_screener",
		"Manually trigger the full-market stock screener.",
		s.handleRunScreener)

	s.addTool("mark_alerts_read",
		"Mark agent alerts as read.",
		s.handleMarkAlertsRead,
		mcp.WithNumber("id", mcp.Description("Alert ID to mark read; omit and set all=true to mark all as read")),
		mcp.WithBoolean("all", mcp.Description("Mark all alerts as read"), mcp.DefaultBool(false)))
}

func (s *Server) registerResources() {
	res := mcp.NewResource("jingzhe://health",
		"Health Status",
		mcp.WithMIMEType("application/json"),
	)
	s.mcp.AddResource(res, func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return s.resourceJSON(s.svc.BuildHealthStatus())
	})

	res = mcp.NewResource("jingzhe://agent/brief",
		"Agent Brief",
		mcp.WithMIMEType("application/json"),
	)
	s.mcp.AddResource(res, func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		brief := s.svc.BuildAgentBrief()
		return s.resourceJSON(brief)
	})
}

// addTool registers a tool with the MCP server.
func (s *Server) addTool(name, description string, handler func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error), opts ...mcp.ToolOption) {
	toolOpts := []mcp.ToolOption{mcp.WithDescription(description)}
	toolOpts = append(toolOpts, opts...)
	tool := mcp.NewTool(name, toolOpts...)
	s.mcp.AddTool(tool, handler)
}

// resourceJSON converts any value to a single JSON text resource content.
func (s *Server) resourceJSON(v interface{}) ([]mcp.ResourceContents, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return []mcp.ResourceContents{mcp.TextResourceContents{
		URI:      "resource",
		MIMEType: "application/json",
		Text:     string(b),
	}}, nil
}

// errorResult builds a tool result that represents an error message.
func (s *Server) errorResult(format string, args ...interface{}) *mcp.CallToolResult {
	return mcp.NewToolResultError(fmt.Sprintf(format, args...))
}

// jsonResult serializes v to JSON and wraps it in a tool result.
func (s *Server) jsonResult(v interface{}) (*mcp.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return s.errorResult("json marshal error: %v", err), nil
	}
	return mcp.NewToolResultText(string(b)), nil
}

// getString returns a string argument with a default fallback.
func (s *Server) getString(args map[string]interface{}, key, defaultVal string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return defaultVal
}

// getNumber returns a float64 argument with a default fallback.
func (s *Server) getNumber(args map[string]interface{}, key string, defaultVal float64) float64 {
	if v, ok := args[key].(float64); ok {
		return v
	}
	if v, ok := args[key].(int); ok {
		return float64(v)
	}
	return defaultVal
}

// getBool returns a bool argument with a default fallback.
func (s *Server) getBool(args map[string]interface{}, key string, defaultVal bool) bool {
	if v, ok := args[key].(bool); ok {
		return v
	}
	return defaultVal
}

// today returns today's date in YYYYMMDD format.
func (s *Server) today() string {
	return time.Now().Format("20060102")
}
