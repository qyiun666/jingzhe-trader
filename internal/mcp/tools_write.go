package mcp

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/ticket"
)

// jobEveningPipeline 收盘后一条流水线任务名（与 scheduler.BuildJobs 注册名一致）。
const jobEveningPipeline = "evening_pipeline"

// registerWriteTools 写工具：人工在券商 App 执行后回报，以及对账本/档位/任务状态的干预。
// 结果一律写在被改动的那一行上（指令单行 / config_kv 的 goal.state），过程只记服务日志；
// 当日「什么成了什么砸了」的统一落点是 run_trace，由调度器与各通道自己写入，这里不另记账。
func (s *Server) registerWriteTools() {
	s.tools["init_day"] = &Tool{
		Name:        "init_day",
		Description: "初始化当日流程：检查数据新鲜度，若当日尚未选股/出信号则补跑（假定数据已就绪）。外部 agent 每日开工第一步。",
		InputSchema: objSchema(map[string]interface{}{"date": dateProp}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			rep, err := s.deps.Freshness.Check(ctx, date)
			if err != nil {
				return nil, err
			}
			if rep.Skipped {
				return map[string]interface{}{
					"date": date, "skipped": true,
					"message": "非交易日，当日没有流程可初始化（不补跑收盘流水线）",
				}, nil
			}
			if !rep.Fresh {
				return map[string]interface{}{"fresh": false, "message": "数据不新鲜，当日不出指令；请先完成行情同步", "detail": rep.String()}, nil
			}
			done, err := s.deps.Store.TraceRepo().HasSucceeded(ctx, model.TraceJob(jobEveningPipeline), date)
			if err != nil {
				return nil, err
			}
			if done {
				return map[string]interface{}{"date": date, "fresh": rep.Fresh, "already_ran": true}, nil
			}
			// 与到点触发走同一条流水线（含行情回补、门禁、档位、选股、决策、写单），
			// 不在这里另拼一套"选股+信号"——那会跑出一套与调度器不同的当日结果。
			if err := s.deps.Jobs.RunNamed(ctx, jobEveningPipeline, date, "manual"); err != nil {
				return nil, fmt.Errorf("补跑收盘流水线失败: %w", err)
			}
			return map[string]interface{}{"date": date, "fresh": rep.Fresh, "already_ran": false, "ran": jobEveningPipeline}, nil
		},
	}
	s.tools["report_fill"] = &Tool{
		Name:        "report_fill",
		Description: "回执一笔成交（外部 agent 在券商 App 人工下单后回报）。必填 qty/price；ticket_id 与 ts_code 至少其一。若按 ts_code 命中多张或多义，返回 need_confirm=true 不记账。",
		InputSchema: objSchema(map[string]interface{}{
			"ticket_id": numProp("指令单 id（优先）"),
			"ts_code":   strProp("证券代码（无 ticket_id 时按当日活跃单匹配）"),
			"date":      dateProp,
			"qty":       numProp("实际成交量（股）"),
			"price":     numProp("实际成交价（元）"),
			"note":      strProp("备注"),
			"actor":     actorProp,
		}, []string{"qty", "price"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			actor := argStr(a, "actor", defaultActor)
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

			res, err := s.deps.Ledger.ReportFill(ctx, ticket.FillRequest{
				TicketID: ticketID, TsCode: tsCode, Qty: model.Qty(qty),
				Price: model.FromFloat(price), Note: note, Actor: actor,
			})
			if err != nil {
				return nil, err
			}
			// 成交留痕就在指令单行上（reported_by/reported_at/note + 成交列），不再另记审计表。
			return map[string]interface{}{
				"need_confirm": false,
				"duplicate":    res.Duplicate,
				"fill":         res.Fill,
			}, nil
		},
	}
	s.tools["sync_portfolio"] = &Tool{
		Name: "sync_portfolio",
		Description: "以券商实际持仓校准账本（首次接入与纠错）。available_cash_yuan 必填：校准进来的持仓" +
			"没有成交单支撑，必须同时给出券商口径的可用现金，系统把它落成现金锚点，锚点当日及之前的成交" +
			"不再重复扣减现金（不给就会把持仓成本双算成可用资金）。initial_capital_yuan 为本金=期初总资产" +
			"（含持仓成本），首次写入，已配置且数值不同时拒绝覆盖，响应里 capital_rejected=true" +
			"（验收 #14），持仓与现金照常同步。价格为元，数量为股。",
		InputSchema: objSchema(map[string]interface{}{
			"date":                 dateProp,
			"initial_capital_yuan": numProp("本金 = 期初总资产（元），0=不改本金"),
			"available_cash_yuan":  numProp("券商口径的可用现金（元），必填且大于 0"),
			"positions":            positionsSchema(),
			"actor":                actorProp,
		}, []string{"positions", "available_cash_yuan"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			actor := argStr(a, "actor", defaultActor)
			rows := argArr(a, "positions")
			items := make([]ticket.PortfolioInput, 0, len(rows))
			for _, r := range rows {
				items = append(items, ticket.PortfolioInput{
					TsCode:       argStr(r, "ts_code", ""),
					TotalQty:     model.Qty(argInt(r, "total_qty", 0)),
					AvailableQty: model.Qty(argInt(r, "available_qty", 0)),
					TodayBought:  model.Qty(argInt(r, "today_bought", 0)),
					CostPrice:    model.FromFloat(argFloat(r, "cost_price", 0)),
					HighPrice:    model.FromFloat(argFloat(r, "high_price", 0)),
				})
			}
			n, rejected, err := s.deps.Ledger.SyncPortfolio(ctx, ticket.PortfolioSync{
				Date:    date,
				Capital: model.FromFloat(argFloat(a, "initial_capital_yuan", 0)),
				Cash:    model.FromFloat(argFloat(a, "available_cash_yuan", 0)),
				Items:   items,
				Actor:   actor,
			})
			if err != nil {
				return nil, err
			}
			out := map[string]interface{}{"synced": n, "date": date}
			if cash, cerr := s.deps.Ledger.Cash(ctx); cerr == nil {
				out["cash_after_sync"] = cash.String()
			} else {
				out["cash_after_sync"] = "读取失败: " + cerr.Error()
			}
			if rejected {
				out["capital_rejected"] = true
				out["message"] = "本金为 write-once，已拒绝覆盖（现有值保持不变，如需修正请走人工复核流程）；持仓与现金已同步"
			}
			return out, nil
		},
	}
	s.tools["skip_ticket"] = &Tool{
		Name:        "skip_ticket",
		Description: "作废一张指令单（置 skipped，终态）。用于人工判断不该执行买入/卖出。必填 reason；drafted 与 issued 可跳，已成交/已过期不可跳。作废结果直接写在指令单行上，reason 记入服务日志。",
		InputSchema: objSchema(map[string]interface{}{
			"ticket_id": numProp("指令单 id"),
			"reason":    strProp("作废原因（必填，写在指令单的 note 上）"),
			"actor":     actorProp,
		}, []string{"ticket_id", "reason"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			id := argInt64(a, "ticket_id", 0)
			if id == 0 {
				return nil, fmt.Errorf("ticket_id 必填")
			}
			reason := argStr(a, "reason", "")
			if reason == "" {
				return nil, fmt.Errorf("作废指令单必须给出 reason（拒绝无理由改状态）")
			}
			actor := argStr(a, "actor", defaultActor)
			if err := s.deps.Tickets.Transition(ctx, id, model.TicketSkipped, actor, reason); err != nil {
				return nil, err
			}
			return map[string]interface{}{"ticket_id": id, "status": string(model.TicketSkipped), "ok": true}, nil
		},
	}
	s.tools["set_gear"] = &Tool{
		Name:        "set_gear",
		Description: "人工覆盖档位（G1/G2/G3）。覆盖解除锁利；until 为空默认当日。结果写在 config_kv 的 goal.state 里（override_gear/override_reason/override_until）。",
		InputSchema: objSchema(map[string]interface{}{
			"gear":   strProp("目标档位 G1/G2/G3"),
			"reason": strProp("原因"),
			"until":  strProp("覆盖有效期 YYYYMMDD，空=当日"),
			"actor":  actorProp,
		}, []string{"gear", "reason"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			gear := model.Gear(argStr(a, "gear", ""))
			if !gear.Valid() {
				return nil, fmt.Errorf("非法档位: %q（应为 G1/G2/G3）", gear)
			}
			res, err := s.deps.Goal.SetGear(ctx, gear, argStr(a, "reason", ""),
				argStr(a, "until", ""), argStr(a, "actor", defaultActor))
			if err != nil {
				return nil, err
			}
			return res, nil
		},
	}
	s.tools["confirm_pace"] = &Tool{
		Name: "confirm_pace",
		Description: "确认当日「激进（aggressive）落后策略」续期（三重保护③）。goal.pace_policy=aggressive 时，" +
			"当日未确认则不放大仓位、回落档位原值；默认策略 unrestricted 不读该确认，调用无实际效果。每日一次、幂等。",
		InputSchema: objSchema(map[string]interface{}{"date": dateProp}, nil),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			if err := s.deps.Goal.ConfirmPace(ctx, date); err != nil {
				return nil, err
			}
			return map[string]interface{}{"date": date, "confirmed": true}, nil
		},
	}
	s.tools["trigger_task"] = &Tool{
		Name: "trigger_task",
		Description: "立即执行一个任务（补跑/调试），任务名见返回的 jobs 字段。与到点触发走同一条轨迹落库路径，" +
			"成功/降级/失败都写当日 run_trace 的那一行。",
		InputSchema: objSchema(map[string]interface{}{
			"task": strProp("任务名（必须是 jobs 列表之一）"),
			"date": dateProp,
		}, []string{"task"}),
		Handler: func(ctx context.Context, a map[string]interface{}) (interface{}, error) {
			date := argStr(a, "date", today())
			task := argStr(a, "task", "")
			jobs := s.deps.Jobs.JobNames()
			if err := s.deps.Jobs.RunNamed(ctx, task, date, "manual"); err != nil {
				return nil, err
			}
			return map[string]interface{}{"task": task, "date": date, "ok": true, "jobs": jobs}, nil
		},
	}
}

// positionsSchema sync_portfolio 的持仓数组元素结构。
func positionsSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "array",
		"items": objSchema(map[string]interface{}{
			"ts_code":       strProp("证券代码，如 600519.SH"),
			"total_qty":     numProp("总持仓（股）"),
			"available_qty": numProp("可卖数量（股），缺省按 total_qty − today_bought 推算"),
			"today_bought":  numProp("当日买入（股）"),
			"cost_price":    numProp("成本价（元）"),
			"high_price":    numProp("持仓期最高价（元），移动止盈基准"),
		}, []string{"ts_code", "total_qty"}),
	}
}
