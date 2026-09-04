package mcp

import (
	"context"

	"jingzhe-trader/internal/model"
)

// registerReadTools 只读工具：外部 agent 用它读到的一切都必须与调度器算出来的一致。
func (s *Server) registerReadTools() {
	s.tools["get_brief"] = &Tool{
		Name:        "get_brief",
		Description: "获取当日总览：数据新鲜度、阻断项(blockers)、候选/信号/持仓摘要、账户快照、目标进度。外部 agent 每日必读的第一道入口。",
		InputSchema: objSchema(map[string]interface{}{"date": dateProp}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			return s.buildBrief(ctx, argStr(a, "date", today()))
		},
	}
	s.tools["get_tickets"] = &Tool{
		Name:        "get_tickets",
		Description: "读取指令单（order_ticket）。外部 agent 据此在券商 App 人工执行买卖。status 可为空（全部）或 drafted/issued/filled/skipped/expired。",
		InputSchema: objSchema(map[string]interface{}{
			"date":   dateProp,
			"status": strProp("筛选状态，空=全部；取值 drafted|issued|filled|skipped|expired"),
		}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			status := argStr(a, "status", "")
			// 白名单校验：非法状态名会静默返回空列表，agent 会误判为"今日无指令"。
			if status != "" && !model.ValidTicketStatus(status) {
				return nil, errBadTicketStatus
			}
			ts, err := s.deps.Store.TradeRepo().ListTickets(ctx, date, status)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"trade_date": date, "tickets": nonNil(ts)}, nil
		},
	}
	s.tools["get_positions"] = &Tool{
		Name:        "get_positions",
		Description: "读取当前持仓快照（position 表）。",
		InputSchema: objSchema(map[string]interface{}{}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			ps, err := s.deps.Store.TradeRepo().ListPositions(ctx)
			if err != nil {
				return nil, err
			}
			views := make([]positionJSON, 0, len(ps))
			for _, pos := range ps {
				views = append(views, positionJSON{
					Position:     pos,
					AvailableQty: int64(pos.Available()),
				})
			}
			return map[string]interface{}{"positions": views}, nil
		},
	}
	s.tools["get_portfolio"] = &Tool{
		Name:        "get_portfolio",
		Description: "当前账户资产：可用资金、持仓市值、总资产、持仓数（由成交与持仓实时推算，不读快照表）+ 当日生效档位。",
		InputSchema: objSchema(map[string]interface{}{}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			ast, err := s.deps.Ledger.Assets(ctx, today())
			if err != nil {
				return nil, err
			}
			out := map[string]interface{}{
				"trade_date":     ast.TradeDate,
				"cash_yuan":      float64(ast.Cash) / 100,
				"market_value":   float64(ast.MarketValue) / 100,
				"total_asset":    float64(ast.TotalAsset) / 100,
				"position_count": ast.PositionCount,
			}
			if gs, gerr := s.deps.Store.GoalRepo().GetGoalState(ctx); gerr == nil {
				out["gear"] = gs.CurrentGear
			}
			return out, nil
		},
	}
	s.tools["get_logs"] = &Tool{
		Name: "get_logs",
		Description: "每日轨迹核查：当日每件事的最终结果，一行一条（subject + outcome=ok/partial/fail + detail）。" +
			"job:任务名 / mail:邮件类型 / alert:告警码 —— 失败与降级都在里面，对应 agent 的'每天查日志'职责。",
		InputSchema: objSchema(map[string]interface{}{"date": dateProp}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			traces, err := s.deps.Store.TraceRepo().List(ctx, date)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"trade_date": date, "trace": nonNil(traces)}, nil
		},
	}
}

// positionJSON 持仓输出：库里不再有 available_qty 列，可卖量在出口处现算附上，
// 免得读接口的 agent 自己去做 total − today 的减法（那是本系统该负责的口径）。
type positionJSON struct {
	model.Position
	AvailableQty int64 `json:"AvailableQty"`
}
