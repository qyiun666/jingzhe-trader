package mcp

import (
	"context"

	"jingzhe-trader/internal/model"
)

// buildBrief 组装当日总览：数据新鲜度、阻断项、指令单与持仓计数、账户资产、目标进度。
// 与诊断不黑盒：任何缺失都显式进入 blockers，便于外部 agent 判断当天能否交易。
func (s *Server) buildBrief(ctx context.Context, date string) (map[string]interface{}, error) {
	out := map[string]interface{}{"trade_date": date}

	blockers := []string{}
	dataFresh := true
	rep, err := s.deps.Freshness.Check(ctx, date)
	if err != nil {
		dataFresh = false
		blockers = append(blockers, "freshness_check_error: "+err.Error())
	} else if !rep.Fresh && !rep.Skipped {
		dataFresh = false
		blockers = append(blockers, "DATA_STALE: 数据不新鲜，今日不应生成/执行任何指令")
	} else if rep.Skipped {
		out["note"] = "非交易日，仅展示既有状态"
	}
	out["data_fresh"] = dataFresh
	out["blockers"] = blockers

	// 候选与信号不落库（选股是一条流水线，中间产物只在内存），
	// 对外可见的结果只有待买卖表与持仓；漏斗每级计数只在日志里。
	tks, err := s.deps.Store.TradeRepo().ListTickets(ctx, date, "")
	if err != nil {
		return nil, err
	}
	pos, err := s.deps.Store.TradeRepo().ListPositions(ctx)
	if err != nil {
		return nil, err
	}
	pending := 0
	for _, t := range tks {
		if t.Status == model.TicketDrafted || t.Status == model.TicketIssued {
			pending++
		}
	}
	out["tickets_total"] = len(tks)
	out["tickets_pending"] = pending
	out["positions"] = len(pos)
	if ran, e := s.deps.Store.TraceRepo().HasSucceeded(ctx, model.TraceJob(jobEveningPipeline), date); e == nil {
		out["pipeline_done"] = ran
	}

	if ast, e := s.deps.Ledger.Assets(ctx, date); e == nil {
		out["portfolio"] = map[string]interface{}{
			"cash_yuan":      float64(ast.Cash) / 100,
			"market_value":   float64(ast.MarketValue) / 100,
			"total_asset":    float64(ast.TotalAsset) / 100,
			"position_count": ast.PositionCount,
		}
	}
	if b, e := s.deps.Goal.Brief(ctx, date); e == nil {
		out["goal"] = b
	}
	return out, nil
}
