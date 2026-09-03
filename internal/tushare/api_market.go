package tushare

import (
	"context"

	"jingzhe-trader/internal/model"
)

// TradeCalRow trade_cal 解码 DTO（model 无对应类型，适配层内部使用）。
type TradeCalRow struct {
	CalDate       string `db:"cal_date"`
	IsOpen        bool   `db:"is_open"`
	PreTradeDate  string `db:"pretrade_date"`
	NextTradeDate string `db:"nexttrade_date"`
	Exchange      string `db:"exchange"`
}

// AdjFactorRow adj_factor 解码 DTO。
type AdjFactorRow struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	AdjFactor float64 `db:"adj_factor"`
}

// TradeCal 拉取交易日历（按交易所单次全量，覆盖至 2099）。
func (c *Client) TradeCal(ctx context.Context, exchange, start, end string) ([]TradeCalRow, error) {
	fields, items, err := c.Call(ctx, "trade_cal",
		map[string]interface{}{"exchange": exchange, "start_date": start, "end_date": end},
		"cal_date", "is_open", "pretrade_date", "next_trade_date")
	if err != nil {
		return nil, err
	}
	var rows []TradeCalRow
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Exchange = exchange
	}
	return rows, nil
}

// StockBasic 拉取全部上市股票基础信息（慢路径 fina 同步的股票列表来源）。
func (c *Client) StockBasic(ctx context.Context) ([]model.StockBasic, error) {
	fields, items, err := c.Call(ctx, "stock_basic",
		map[string]interface{}{"exchange": "", "list_status": "L"},
		"ts_code", "symbol", "name", "market", "exchange", "industry", "list_date", "delist_date", "list_status")
	if err != nil {
		return nil, err
	}
	var rows []model.StockBasic
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Daily 按交易日全市场拉取日线（单次调用，约 5500+ 行）。
// 价格为未复权原始值，前复权口径由 dataloader 结合 adj_factor 计算。
func (c *Client) Daily(ctx context.Context, tradeDate string) ([]model.Bar, error) {
	fields, items, err := c.Call(ctx, "daily",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code", "trade_date", "open", "high", "low", "close", "pre_close", "vol", "amount", "pct_chg")
	if err != nil {
		return nil, err
	}
	var raw []RawBar
	if err := DecodeItems(fields, items, &raw); err != nil {
		return nil, err
	}
	out := make([]model.Bar, 0, len(raw))
	for _, r := range raw {
		out = append(out, ToModelBar(r))
	}
	return out, nil
}

// DailyBasic 按交易日全市场拉取每日指标。total_mv/circ_mv 千元在转换中折算为万元。
func (c *Client) DailyBasic(ctx context.Context, tradeDate string) ([]model.DailyBasic, error) {
	fields, items, err := c.Call(ctx, "daily_basic",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code", "trade_date", "close", "turnover_rate", "volume_ratio", "pe", "pe_ttm", "pb", "ps_ttm", "dv_ratio", "total_mv", "circ_mv")
	if err != nil {
		return nil, err
	}
	var raw []RawDailyBasic
	if err := DecodeItems(fields, items, &raw); err != nil {
		return nil, err
	}
	out := make([]model.DailyBasic, 0, len(raw))
	for _, r := range raw {
		out = append(out, ToModelDailyBasic(r))
	}
	return out, nil
}

// AdjFactor 按交易日全市场拉取复权因子。
func (c *Client) AdjFactor(ctx context.Context, tradeDate string) ([]AdjFactorRow, error) {
	fields, items, err := c.Call(ctx, "adj_factor",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code", "trade_date", "adj_factor")
	if err != nil {
		return nil, err
	}
	var rows []AdjFactorRow
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// StkLimit 按交易日全市场拉取涨跌停价（涨停禁买/跌停禁卖唯一判定依据，不依赖状态编码猜测）。
func (c *Client) StkLimit(ctx context.Context, tradeDate string) ([]model.PriceLimit, error) {
	fields, items, err := c.Call(ctx, "stk_limit",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code", "trade_date", "up_limit", "down_limit")
	if err != nil {
		return nil, err
	}
	var rows []model.PriceLimit
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Suspend 按交易日全市场拉取停牌信息。
func (c *Client) Suspend(ctx context.Context, tradeDate string) ([]model.Suspend, error) {
	fields, items, err := c.Call(ctx, "suspend_d",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code", "trade_date", "suspend_type", "suspend_timing")
	if err != nil {
		return nil, err
	}
	var rows []model.Suspend
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// IndexDaily 按交易日 + 指数列表拉取大盘日线（ts_code 逗号拼接，单调用）。
func (c *Client) IndexDaily(ctx context.Context, tradeDate string, indexCodes []string) ([]model.IndexDaily, error) {
	tsParam := ""
	for i, c0 := range indexCodes {
		if i > 0 {
			tsParam += ","
		}
		tsParam += c0
	}
	fields, items, err := c.Call(ctx, "index_daily",
		map[string]interface{}{"ts_code": tsParam, "trade_date": tradeDate},
		"ts_code", "trade_date", "close", "open", "high", "low", "pre_close", "pct_chg")
	if err != nil {
		return nil, err
	}
	var rows []model.IndexDaily
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// MoneyFlow 按交易日全市场拉取个股资金流。
func (c *Client) MoneyFlow(ctx context.Context, tradeDate string) ([]model.MoneyFlow, error) {
	fields, items, err := c.Call(ctx, "moneyflow",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code", "trade_date", "buy_elg_amount", "sell_elg_amount", "net_mf_amount")
	if err != nil {
		return nil, err
	}
	var rows []model.MoneyFlow
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
