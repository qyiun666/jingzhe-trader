package mcp

import (
	"context"
)

// buildBrief 组装当日总览：数据新鲜度、阻断项、候选/信号/持仓计数、账户快照、目标进度。
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

	cands, err := s.deps.Store.ScreenRepo().ListScreenResults(ctx, date)
	if err != nil {
		return nil, err
	}
	sigs, err := s.deps.Store.DecisionRepo().ListSignals(ctx, date)
	if err != nil {
		return nil, err
	}
	pos, err := s.deps.Store.TradeRepo().ListPositions(ctx)
	if err != nil {
		return nil, err
	}
	out["candidates"] = len(cands)
	out["signals"] = len(sigs)
	out["positions"] = len(pos)

	if sn, e := s.deps.Store.TradeRepo().LatestSnapshot(ctx); e == nil {
		out["portfolio"] = map[string]interface{}{
			"cash_yuan":      float64(sn.Cash) / 100,
			"market_value":   float64(sn.MarketValue) / 100,
			"total_asset":    float64(sn.TotalAsset) / 100,
			"position_count": sn.PositionCount,
			"gear":           sn.Gear,
		}
	}
	if b, e := s.deps.Goal.Brief(ctx, date); e == nil {
		out["goal"] = b
	}
	return out, nil
}
