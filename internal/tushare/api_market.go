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

// StockBasic 拉取全部上市股票基础信息（1 次调用返回全市场清单）。
func (c *Client) StockBasic(ctx context.Context) ([]model.StockBasic, error) {
	fields, items, err := c.Call(ctx, "stock_basic",
		map[string]interface{}{"exchange": "", "list_status": "L"},
		"ts_code", "name", "industry", "list_date", "list_status")
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
		"ts_code", "trade_date", "close", "vol")
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

// DailyBasic 按交易日全市场拉取估值截面。circ_mv 千元在转换中折算为万元。
//
// 不取 close：一手价用 daily_bar.raw_close（同一天的真实价），两处各存一份必然漂。
func (c *Client) DailyBasic(ctx context.Context, tradeDate string) ([]model.Valuation, error) {
	fields, items, err := c.Call(ctx, "daily_basic",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code", "turnover_rate", "pe_ttm", "pb", "circ_mv")
	if err != nil {
		return nil, err
	}
	var raw []RawValuation
	if err := DecodeItems(fields, items, &raw); err != nil {
		return nil, err
	}
	out := make([]model.Valuation, 0, len(raw))
	for _, r := range raw {
		out = append(out, ToModelValuation(r))
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

// SuspendRow 停牌接口原始解码结构：只取代码一列，日期由入参限定。
type SuspendRow struct {
	TsCode string `db:"ts_code"`
}

// Suspend 按交易日全市场拉取停牌代码。返回代码切片：停牌已经不是一个"行"，
// 只是当日的一个代码集合（见 store.MarketRepo.SaveSuspended）。
func (c *Client) Suspend(ctx context.Context, tradeDate string) ([]string, error) {
	fields, items, err := c.Call(ctx, "suspend_d",
		map[string]interface{}{"trade_date": tradeDate},
		"ts_code")
	if err != nil {
		return nil, err
	}
	var rows []SuspendRow
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	codes := make([]string, 0, len(rows))
	for _, r := range rows {
		codes = append(codes, r.TsCode)
	}
	return codes, nil
}

// IndexDaily 按交易日拉取大盘指数日线（每个指数一次调用），与个股共用 daily_bar 结构。
//
// 实测：ts_code 传逗号拼接的多指数会得到 code=0 + 0 行（不报错、静默为空），
// 因此只能逐指数调用（6 个指数 = 6 次调用/天，配额占比可忽略）。
func (c *Client) IndexDaily(ctx context.Context, tradeDate string, indexCodes []string) ([]model.Bar, error) {
	var out []model.Bar
	for _, code := range indexCodes {
		fields, items, err := c.Call(ctx, "index_daily",
			map[string]interface{}{"ts_code": code, "trade_date": tradeDate},
			"ts_code", "trade_date", "close")
		if err != nil {
			return nil, err
		}
		var rows []model.Bar
		if err := DecodeItems(fields, items, &rows); err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}
