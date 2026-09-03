package tushare

import (
	"context"

	"jingzhe-trader/internal/model"
)

// FinaIndicator 按 ts_code 拉取财务指标（慢路径，需逐只查询）。
//
// 注意：fina_indicator 无全市场批量入口，必须逐只调用，故由 dataloader 控制
// 限流（tushare.Client 内部令牌桶）/ 指数退避（Call 内部）/ 游标断点续传。
// 严禁在每日主流程里做全量 fina 刷新。
func (c *Client) FinaIndicator(ctx context.Context, tsCode string) ([]model.FinaIndicator, error) {
	fields, items, err := c.Call(ctx, "fina_indicator",
		map[string]interface{}{"ts_code": tsCode},
		"ts_code", "end_date", "ann_date", "eps", "roe", "roe_dt",
		"grossprofit_margin", "netprofit_margin", "debt_to_assets",
		"netprofit_yoy", "or_yoy", "bps")
	if err != nil {
		return nil, err
	}
	var rows []model.FinaIndicator
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
