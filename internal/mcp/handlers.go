package mcp

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"

	"jingzhe-trader/internal/api"
)

// ---------- read tools ----------

func (s *Server) handleGetHealth(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.jsonResult(s.svc.BuildHealthStatus())
}

func (s *Server) handleGetAgentBrief(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.jsonResult(s.svc.BuildAgentBrief())
}

func (s *Server) handleGetAgentDashboard(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.jsonResult(s.svc.BuildAgentDashboard())
}

func (s *Server) handleGetAgentChanges(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	date := s.getString(req.GetArguments(), "date", "")
	return s.jsonResult(s.svc.BuildAgentChanges(date))
}

func (s *Server) handleGetAgentAlerts(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	unreadOnly := s.getBool(req.GetArguments(), "unread_only", false)
	date := s.getString(req.GetArguments(), "date", "")
	return s.jsonResult(s.svc.BuildAgentAlerts(unreadOnly, date))
}

func (s *Server) handleGetPositions(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	date := s.getString(req.GetArguments(), "date", s.today())
	portfolio, err := s.svc.RunPositions(date)
	if err != nil {
		return s.errorResult("run positions failed: %v", err), nil
	}
	return s.jsonResult(portfolio)
}

func (s *Server) handleGetPortfolio(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.jsonResult(s.svc.BuildPortfolio())
}

func (s *Server) handleGetMarket(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	date := s.getString(req.GetArguments(), "date", s.today())
	market, err := s.svc.RunMarket(date)
	if err != nil {
		return s.errorResult("run market failed: %v", err), nil
	}
	return s.jsonResult(market)
}

func (s *Server) handleGetDailyReport(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	date := s.getString(req.GetArguments(), "date", s.today())
	strategy := s.getString(req.GetArguments(), "strategy", "")
	if strategy == "" {
		strategy = s.svc.SelectStrategy(date)
	}
	report, err := s.svc.RunDaily(date, strategy)
	if err != nil {
		return s.errorResult("run daily report failed: %v", err), nil
	}
	return s.jsonResult(report)
}

func (s *Server) handleGetPlans(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	date := s.getString(req.GetArguments(), "date", "")
	return s.jsonResult(s.svc.BuildPlans(date))
}

func (s *Server) handleGetStrategyStatus(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	status, err := s.svc.BuildStrategyStatus()
	if err != nil {
		return s.errorResult("strategy status failed: %v", err), nil
	}
	return s.jsonResult(status)
}

func (s *Server) handleGetGoalStatus(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	date := s.getString(req.GetArguments(), "date", s.today())
	st, err := s.svc.GoalStatus(date)
	if err != nil {
		return s.errorResult("goal status failed: %v", err), nil
	}
	return s.jsonResult(st)
}

func (s *Server) handleGetScreenerResults(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	date := s.getString(req.GetArguments(), "date", "")
	return s.jsonResult(s.svc.BuildScreenerResults(date))
}

func (s *Server) handleGetSystemStatus(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return s.jsonResult(s.svc.BuildSystemStatus())
}

func (s *Server) handleGetKline(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	code := s.getString(req.GetArguments(), "code", "")
	start := s.getString(req.GetArguments(), "start", "20200101")
	end := s.getString(req.GetArguments(), "end", s.today())
	bars, err := s.svc.GetKline(code, start, end)
	if err != nil {
		return s.errorResult("kline failed: %v", err), nil
	}
	return s.jsonResult(bars)
}

func (s *Server) handleGetSnapshots(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	limit := int(s.getNumber(req.GetArguments(), "limit", 30))
	if limit <= 0 {
		limit = 30
	}
	return s.jsonResult(s.svc.BuildSnapshots(limit))
}

// ---------- write tools ----------

func (s *Server) handleConfirmPlan(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := int64(s.getNumber(req.GetArguments(), "id", 0))
	if id <= 0 {
		return s.errorResult("id is required"), nil
	}
	plan, err := s.svc.ConfirmPlan(id)
	if err != nil {
		return s.errorResult("confirm plan failed: %v", err), nil
	}
	return s.jsonResult(plan)
}

func (s *Server) handleConfirmTrade(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	tr := api.TradeConfirmRequest{
		TsCode: s.getString(req.GetArguments(), "ts_code", ""),
		Side:   s.getString(req.GetArguments(), "side", ""),
		Qty:    int(s.getNumber(req.GetArguments(), "qty", 0)),
		Price:  s.getNumber(req.GetArguments(), "price", 0),
		PlanID: int64(s.getNumber(req.GetArguments(), "plan_id", 0)),
	}
	resp, err := s.svc.ConfirmTrade(tr)
	if err != nil {
		return s.errorResult("confirm trade failed: %v", err), nil
	}
	return s.jsonResult(resp)
}

func (s *Server) handleSyncPortfolio(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	positionsArg, ok := req.GetArguments()["positions"].([]interface{})
	if !ok {
		return s.errorResult("positions must be an array"), nil
	}
	var positions []api.SyncPositionItem
	for _, p := range positionsArg {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		positions = append(positions, api.SyncPositionItem{
			TsCode:       getMapString(m, "ts_code"),
			TotalQty:     int(getMapNumber(m, "total_qty")),
			AvailableQty: int(getMapNumber(m, "available_qty")),
			CostPrice:    getMapNumber(m, "cost_price"),
		})
	}
	overwrite := s.getBool(req.GetArguments(), "overwrite", true)
	cash := s.getNumber(req.GetArguments(), "cash", 0)
	reqObj := api.SyncPortfolioRequest{
		Positions: positions,
		Cash:      cash,
		Overwrite: &overwrite,
	}
	resp, err := s.svc.SyncPortfolio(reqObj)
	if err != nil {
		return s.errorResult("sync portfolio failed: %v", err), nil
	}
	return s.jsonResult(resp)
}

func (s *Server) handleUpdateData(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if err := s.svc.UpdateData(); err != nil {
		return s.errorResult("update data failed: %v", err), nil
	}
	return s.jsonResult(map[string]string{"message": "data update triggered"})
}

func (s *Server) handleSwitchStrategy(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := s.getString(req.GetArguments(), "strategy", "")
	result, err := s.svc.SwitchStrategy(name)
	if err != nil {
		return s.errorResult("switch strategy failed: %v", err), nil
	}
	return s.jsonResult(result)
}

func (s *Server) handleRunScreener(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	result, err := s.svc.RunScreener()
	if err != nil {
		return s.errorResult("run screener failed: %v", err), nil
	}
	return s.jsonResult(result)
}

func (s *Server) handleMarkAlertsRead(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	all := s.getBool(req.GetArguments(), "all", false)
	id := int64(s.getNumber(req.GetArguments(), "id", 0))
	result, err := s.svc.MarkAlertsRead(all, id)
	if err != nil {
		return s.errorResult("mark alerts read failed: %v", err), nil
	}
	return s.jsonResult(result)
}

// getMapString extracts a string value from a generic map.
func getMapString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// getMapNumber extracts a float64 value from a generic map.
func getMapNumber(m map[string]interface{}, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	if v, ok := m[key].(int); ok {
		return float64(v)
	}
	return 0
}

// ensure that tool handlers satisfy the expected signatures.
var _ = (func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error))(nil)
