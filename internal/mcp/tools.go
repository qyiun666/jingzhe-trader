package mcp

import (
	"context"
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/ticket"
)

// Tool 单个 MCP 工具定义与处理函数。
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	Handler     func(ctx context.Context, args map[string]interface{}) (interface{}, error)
}

func objSchema(props map[string]interface{}, required []string) map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": props, "required": required}
}

func strProp(desc string) map[string]interface{} { return map[string]interface{}{"type": "string", "description": desc} }
func numProp(desc string) map[string]interface{} { return map[string]interface{}{"type": "number", "description": desc} }

func (s *Server) registerTools() {
	s.tools["get_brief"] = &Tool{
		Name:        "get_brief",
		Description: "获取当日总览：数据新鲜度、阻断项(blockers)、候选/信号/持仓摘要、目标进度。外部 agent 每日必读的第一道入口。",
		InputSchema: objSchema(map[string]interface{}{"date": strProp("交易日 YYYYMMDD，缺省为今天")}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			return s.buildBrief(ctx, argStr(a, "date", today()))
		},
	}
	s.tools["get_candidates"] = &Tool{
		Name:        "get_candidates",
		Description: "读取当日选股候选（screen_result，含五因子分项与理由）与漏斗诊断（screen_funnel）。",
		InputSchema: objSchema(map[string]interface{}{
			"date":  strProp("交易日 YYYYMMDD，缺省为今天"),
			"limit": numProp("返回前 N 条候选（按排名），0=全部"),
		}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			rows, err := s.deps.Store.ScreenRepo().ListScreenResults(ctx, date)
			if err != nil {
				return nil, err
			}
			funnel, _ := s.deps.Store.ScreenRepo().ListFunnel(ctx, date)
			lim := argInt(a, "limit", 0)
			if lim > 0 && len(rows) > lim {
				rows = rows[:lim]
			}
			return map[string]interface{}{"trade_date": date, "candidates": rows, "funnel": funnel}, nil
		},
	}
	s.tools["get_signals"] = &Tool{
		Name:        "get_signals",
		Description: "读取当日信号（signal 表：买入/卖出、规则、状态）。",
		InputSchema: objSchema(map[string]interface{}{"date": strProp("交易日 YYYYMMDD，缺省为今天")}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			sigs, err := s.deps.Store.DecisionRepo().ListSignals(ctx, date)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"trade_date": date, "signals": sigs}, nil
		},
	}
	s.tools["get_tickets"] = &Tool{
		Name:        "get_tickets",
		Description: "读取指令单（order_ticket）。外部 agent 据此在券商 App 人工执行买卖。status 可为空（全部）或 drafted/issued/filled/closed/expired/rejected。",
		InputSchema: objSchema(map[string]interface{}{
			"date":   strProp("交易日 YYYYMMDD，缺省为今天"),
			"status": strProp("筛选状态，空=全部"),
		}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			status := argStr(a, "status", "")
			ts, err := s.deps.Store.TradeRepo().ListTickets(ctx, date, status)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"trade_date": date, "tickets": ts}, nil
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
			return map[string]interface{}{"positions": ps}, nil
		},
	}
	s.tools["get_portfolio"] = &Tool{
		Name:        "get_portfolio",
		Description: "读取账户快照（account_snapshot）：现金、市值、总资产、持仓数、当前档位。",
		InputSchema: objSchema(map[string]interface{}{"date": strProp("交易日 YYYYMMDD，缺省取最新快照")}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			sn, err := s.deps.Store.TradeRepo().LatestSnapshot(ctx)
			if err != nil {
				return nil, err
			}
			return sn, nil
		},
	}
	s.tools["get_logs"] = &Tool{
		Name:        "get_logs",
		Description: "每日日志核查：当日任务执行记录（job_run，含 degraded/failed）与全部告警（agent_alert）。对应 agent 的'每天查日志'职责。",
		InputSchema: objSchema(map[string]interface{}{"date": strProp("交易日 YYYYMMDD，缺省为今天")}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			jobs, err := s.deps.Store.OpsRepo().ListJobRuns(ctx, date)
			if err != nil {
				return nil, err
			}
			alerts, err := s.deps.Store.OpsRepo().ListAlerts(ctx, date)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{"trade_date": date, "jobs": jobs, "alerts": alerts}, nil
		},
	}
	s.tools["report_fill"] = &Tool{
		Name:        "report_fill",
		Description: "回执一笔成交（外部 agent 在券商 App 人工下单后回报）。必填 qty/price；ticket_id 与 ts_code 至少其一。若按 ts_code 命中多张或多义，返回 need_confirm=true 不记账。",
		InputSchema: objSchema(map[string]interface{}{
			"ticket_id": numProp("指令单 id（优先）"),
			"ts_code":   strProp("证券代码（无 ticket_id 时按当日活跃单匹配）"),
			"qty":       numProp("实际成交量（股）"),
			"price":     numProp("实际成交价（元）"),
			"note":      strProp("备注"),
			"actor":     strProp("操作者，缺省 mcp-agent"),
		}, []string{"qty", "price"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			actor := argStr(a, "actor", "mcp-agent")
			qty := argInt(a, "qty", 0)
			price := argFloat(a, "price", 0)
			note := argStr(a, "note", "")
			ticketID := argInt64(a, "ticket_id", 0)
			tsCode := argStr(a, "ts_code", "")

			if ticketID == 0 && tsCode == "" {
				return nil, fmt.Errorf("ticket_id 与 ts_code 至少提供一个")
			}
			// 未给 ticket_id 时按 ts_code 在当日活跃单中精确匹配
			if ticketID == 0 {
				acts, err := s.deps.Store.TradeRepo().ListActiveTickets(ctx, date)
				if err != nil {
					return nil, err
				}
				var match []model.OrderTicket
				for _, t := range acts {
					if t.TsCode == tsCode {
						match = append(match, t)
					}
				}
				switch len(match) {
				case 1:
					ticketID = match[0].ID
				default:
					cands := make([]map[string]interface{}, 0, len(match))
					for _, m := range match {
						cands = append(cands, map[string]interface{}{"ticket_id": m.ID, "ts_code": m.TsCode, "direction": string(m.Direction), "qty": int64(m.Qty)})
					}
					return map[string]interface{}{
						"need_confirm": true,
						"message":      fmt.Sprintf("按 %s 命中 %d 张活跃指令单，无法唯一确定，请指定 ticket_id", tsCode, len(match)),
						"candidates":   cands,
					}, nil
				}
			}

			req := fillRequest(ticketID, tsCode, qty, price, note, actor)
			res, err := s.deps.Ledger.ReportFill(ctx, req)
			if err != nil {
				return nil, err
			}
			_ = s.logAction(ctx, date, actor, "fill", fmt.Sprintf("%d", ticketID), "report_fill", "",
				fmt.Sprintf("qty=%d price=%.2f dup=%v", qty, price, res.Duplicate), note)
			return map[string]interface{}{
				"need_confirm": false,
				"duplicate":    res.Duplicate,
				"fill":        res.Fill,
			}, nil
		},
	}
	s.tools["init_day"] = &Tool{
		Name:        "init_day",
		Description: "初始化当日流程：检查数据新鲜度，若当日尚未选股/出信号则补跑（假定数据已就绪）。外部 agent 每日开工第一步。",
		InputSchema: objSchema(map[string]interface{}{"date": strProp("交易日 YYYYMMDD，缺省为今天")}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			rep, err := s.deps.Freshness.Check(ctx, date)
			if err != nil {
				return nil, err
			}
			if !rep.Fresh {
				return map[string]interface{}{"fresh": false, "message": "数据不新鲜，无法初始化；请先由调度器完成数据同步", "detail": rep.String()}, nil
			}
			steps := []string{}
			if rows, _ := s.deps.Store.ScreenRepo().ListScreenResults(ctx, date); len(rows) == 0 {
				if _, e := s.deps.Screen.Run(ctx, date); e != nil {
					return nil, fmt.Errorf("选股补跑失败: %w", e)
				}
				steps = append(steps, "screener")
			}
			if n, _ := s.deps.Store.DecisionRepo().CountSignals(ctx, date); n == 0 {
				rp, gear := riskParamsFrom(ctx, s.deps.Config, s.deps.Store)
				if _, e := s.deps.Signal.Generate(ctx, date, rp, gear, signal.PassThroughConfirmer{}); e != nil {
					return nil, fmt.Errorf("信号补跑失败: %w", e)
				}
				steps = append(steps, "signal")
			}
			return map[string]interface{}{"date": date, "fresh": true, "steps": steps}, nil
		},
	}
	s.tools["set_gear"] = &Tool{
		Name:        "set_gear",
		Description: "人工覆盖档位（G1/G2/G3）。覆盖解除锁利；until 为空默认当日。会写 goal_gear_log 与 action_log。",
		InputSchema: objSchema(map[string]interface{}{
			"gear":   strProp("目标档位 G1/G2/G3"),
			"reason": strProp("原因"),
			"until":  strProp("覆盖有效期 YYYYMMDD，空=当日"),
			"actor":  strProp("操作者，缺省 mcp-agent"),
		}, []string{"gear", "reason"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			gear := model.Gear(argStr(a, "gear", ""))
			if !gear.Valid() {
				return nil, fmt.Errorf("非法档位: %q（应为 G1/G2/G3）", gear)
			}
			reason := argStr(a, "reason", "")
			until := argStr(a, "until", "")
			actor := argStr(a, "actor", "mcp-agent")
			res, err := s.deps.Goal.SetGear(ctx, gear, reason, until, actor)
			if err != nil {
				return nil, err
			}
			_ = s.logAction(ctx, date, actor, "goal_state", "gear", "set_gear", "",
				fmt.Sprintf("to=%s until=%s", gear, until), reason)
			return res, nil
		},
	}
	s.tools["note"] = &Tool{
		Name:        "note",
		Description: "追加一条操作日志到 action_log（审计留痕，不改动业务数据）。外部 agent 记录人工决策/备注。",
		InputSchema: objSchema(map[string]interface{}{
			"object_type": strProp("对象类型，如 ticket/fill/position"),
			"object_id":   strProp("对象 id"),
			"action":      strProp("动作，缺省 note"),
			"reason":      strProp("原因/备注"),
			"actor":       strProp("操作者，缺省 mcp-agent"),
		}, []string{"reason"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			actor := argStr(a, "actor", "mcp-agent")
			ot := argStr(a, "object_type", "")
			oid := argStr(a, "object_id", "")
			action := argStr(a, "action", "note")
			reason := argStr(a, "reason", "")
			if err := s.logAction(ctx, date, actor, ot, oid, action, "", "", reason); err != nil {
				return nil, err
			}
			return map[string]interface{}{"ok": true}, nil
		},
	}
	s.tools["ack_alert"] = &Tool{
		Name:        "ack_alert",
		Description: "标记某条告警已读（外部 agent 处理完毕后调用）。",
		InputSchema: objSchema(map[string]interface{}{"alert_id": numProp("告警 id")}, []string{"alert_id"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			id := argInt64(a, "alert_id", 0)
			if id == 0 {
				return nil, fmt.Errorf("alert_id 必填")
			}
			if err := s.deps.Store.OpsRepo().MarkAlertRead(ctx, id); err != nil {
				return nil, err
			}
			return map[string]interface{}{"alert_id": id, "ok": true}, nil
		},
	}
	s.tools["trigger_task"] = &Tool{
		Name:        "trigger_task",
		Description: "手动触发一个命名任务：freshness / screener / signal / t1_settle。补跑或调试用。",
		InputSchema: objSchema(map[string]interface{}{
			"task": strProp("任务名：freshness|screener|signal|t1_settle"),
			"date": strProp("交易日 YYYYMMDD，缺省为今天"),
		}, []string{"task"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			task := argStr(a, "task", "")
			actor := "mcp-agent"
			switch task {
			case "freshness":
				rep, err := s.deps.Freshness.Check(ctx, date)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{"task": task, "fresh": rep.Fresh, "detail": rep.String()}, nil
			case "screener":
				rep, err := s.deps.Screen.Run(ctx, date)
				if err != nil {
					return nil, err
				}
				_ = s.logAction(ctx, date, actor, "screen_result", date, "trigger_task", "", "screener", "")
				return rep, nil
			case "signal":
				rp, gear := riskParamsFrom(ctx, s.deps.Config, s.deps.Store)
				rep, err := s.deps.Signal.Generate(ctx, date, rp, gear, signal.PassThroughConfirmer{})
				if err != nil {
					return nil, err
				}
				_ = s.logAction(ctx, date, actor, "signal", date, "trigger_task", "", "signal", "")
				return rep, nil
			case "t1_settle":
				n, err := s.deps.Ledger.SettleT1(ctx, date)
				if err != nil {
					return nil, err
				}
				return map[string]interface{}{"task": task, "settled": n}, nil
			default:
				return nil, fmt.Errorf("未知任务: %q（支持 freshness/screener/signal/t1_settle）", task)
			}
		},
	}
}

// fillRequest 由参数为回执构造 FillRequest（价格由元换算成分）。
func fillRequest(ticketID int64, tsCode string, qty int, price float64, note, actor string) ticket.FillRequest {
	return ticket.FillRequest{
		TicketID: ticketID,
		TsCode:   tsCode,
		Qty:      model.Qty(qty),
		Price:    model.FromFloat(price),
		Note:     note,
		Actor:    actor,
	}
}

// logAction 写入一条 action_log（审计留痕，验收 §10.6-6）。
func (s *Server) logAction(ctx context.Context, date, actor, objType, objID, action, before, after, reason string) error {
	return s.deps.Store.OpsRepo().InsertActionLog(ctx, model.ActionLog{
		TradeDate:  date,
		Actor:      actor,
		ObjectType: objType,
		ObjectID:   objID,
		Action:     action,
		BeforeValue: before,
		AfterValue:  after,
		Reason:     reason,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
	})
}

// ---- 参数解析辅助 ----

func today() string { return time.Now().Format("20060102") }

func argStr(a map[string]interface{}, k, def string) string {
	if v, ok := a[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func argInt(a map[string]interface{}, k string, def int) int {
	if v, ok := a[k]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := fmt.Sscanf(n, "%d", new(int)); err == nil {
				return i
			}
		}
	}
	return def
}

func argInt64(a map[string]interface{}, k string, def int64) int64 {
	if v, ok := a[k]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		}
	}
	return def
}

func argFloat(a map[string]interface{}, k string, def float64) float64 {
	if v, ok := a[k]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}
