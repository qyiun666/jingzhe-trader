package tushare

import (
	"strings"

	"jingzhe-trader/internal/model"
)

// callAPI 调用指定接口并将 items 转为按字段名索引的行(map)切片
// 这样无论 fields 返回顺序如何, 都能稳定地按字段名取值
func (c *Client) callAPI(apiName string, params map[string]interface{}, fields []string) ([]map[string]interface{}, error) {
	fieldsStr := strings.Join(fields, ",")
	resp, err := c.call(apiName, params, fieldsStr)
	if err != nil {
		return nil, err
	}
	return rowsToMaps(resp), nil
}

// fetchRows 调用接口并将每行映射为 T (统一 15 个数据接口的样板)
func fetchRows[T any](c *Client, apiName string, params map[string]interface{}, fields []string, mapper func(map[string]interface{}) T) ([]T, error) {
	rows, err := c.callAPI(apiName, params, fields)
	if err != nil {
		return nil, err
	}
	result := make([]T, 0, len(rows))
	for _, row := range rows {
		result = append(result, mapper(row))
	}
	return result, nil
}

// setOpt 可选参数: 非空才放入 params
func setOpt(params map[string]interface{}, key, val string) {
	if val != "" {
		params[key] = val
	}
}

// rowsToMaps 将 Tushare 的列存结果(fields + items)转为行 map
func rowsToMaps(resp *Response) []map[string]interface{} {
	fields := resp.Data.Fields
	rows := make([]map[string]interface{}, 0, len(resp.Data.Items))
	for _, item := range resp.Data.Items {
		m := make(map[string]interface{}, len(fields))
		for i, f := range fields {
			if i < len(item) {
				m[f] = item[i]
			} else {
				m[f] = nil
			}
		}
		rows = append(rows, m)
	}
	return rows
}

// 以下为按字段名取值的辅助函数, 统一处理类型兼容
func fieldStr(row map[string]interface{}, field string) string {
	return parseString(row[field])
}

func fieldFloat(row map[string]interface{}, field string) float64 {
	return parseFloat(row[field])
}

func fieldInt(row map[string]interface{}, field string) int {
	return parseInt(row[field])
}

// parseBar 从一行数据解析 K 线
func parseBar(row map[string]interface{}) model.Bar {
	return model.Bar{
		TsCode:    fieldStr(row, "ts_code"),
		TradeDate: fieldStr(row, "trade_date"),
		Open:      fieldFloat(row, "open"),
		High:      fieldFloat(row, "high"),
		Low:       fieldFloat(row, "low"),
		Close:     fieldFloat(row, "close"),
		PreClose:  fieldFloat(row, "pre_close"),
		Change:    fieldFloat(row, "change"),
		PctChg:    fieldFloat(row, "pct_chg"),
		Vol:       fieldFloat(row, "vol"),
		Amount:    fieldFloat(row, "amount"),
		AdjFactor: fieldFloat(row, "adj_factor"),
	}
}

// dailyFields 日线接口请求的字段(daily 接口不返回复权因子)
var dailyFields = []string{
	"ts_code", "trade_date", "open", "high", "low", "close",
	"pre_close", "change", "pct_chg", "vol", "amount",
}

// barParams 按交易日/按代码查询日线的公共参数
func barParams(tradeDate, tsCode, startDate, endDate string) map[string]interface{} {
	params := map[string]interface{}{}
	setOpt(params, "trade_date", tradeDate)
	setOpt(params, "ts_code", tsCode)
	setOpt(params, "start_date", startDate)
	setOpt(params, "end_date", endDate)
	return params
}

// StockBasic 获取 A 股股票列表(基础信息)
func (c *Client) StockBasic() ([]model.Stock, error) {
	fields := []string{
		"ts_code", "symbol", "name", "market", "exchange",
		"is_st", "list_status", "list_date", "delist_date", "industry",
	}
	return fetchRows(c, "stock_basic", map[string]interface{}{}, fields, func(row map[string]interface{}) model.Stock {
		return model.Stock{
			TsCode:     fieldStr(row, "ts_code"),
			Symbol:     fieldStr(row, "symbol"),
			Name:       fieldStr(row, "name"),
			Market:     fieldStr(row, "market"),
			Exchange:   fieldStr(row, "exchange"),
			IsST:       fieldInt(row, "is_st") != 0,
			ListStatus: fieldStr(row, "list_status"),
			ListDate:   fieldStr(row, "list_date"),
			DelistDate: fieldStr(row, "delist_date"),
			Industry:   fieldStr(row, "industry"),
		}
	})
}

// TradeCal 获取指定交易所在 [startDate, endDate] 区间内的交易日历
// exchange: SSE(上交所) / SZSE(深交所) / CFFEX 等
func (c *Client) TradeCal(exchange, startDate, endDate string) ([]model.TradeCal, error) {
	fields := []string{"cal_date", "is_open", "pretrade_date", "exchange"}
	params := map[string]interface{}{
		"exchange":   exchange,
		"start_date": startDate,
		"end_date":   endDate,
	}
	return fetchRows(c, "trade_cal", params, fields, func(row map[string]interface{}) model.TradeCal {
		return model.TradeCal{
			CalDate:      fieldStr(row, "cal_date"),
			IsOpen:       fieldInt(row, "is_open"),
			PreTradeDate: fieldStr(row, "pretrade_date"),
			PExchange:    fieldStr(row, "exchange"),
		}
	})
}

// FundDaily 按交易日获取全市场基金(含ETF)日线(未复权)
// 返回数据结构与 Daily 相同, 可直接写入 daily_bar 表
func (c *Client) FundDaily(tradeDate string) ([]model.Bar, error) {
	return fetchRows(c, "fund_daily", barParams(tradeDate, "", "", ""), dailyFields, parseBar)
}

// Daily 按交易日获取全市场日线(未复权)
func (c *Client) Daily(tradeDate string) ([]model.Bar, error) {
	return fetchRows(c, "daily", barParams(tradeDate, "", "", ""), dailyFields, parseBar)
}

// DailyByCode 按股票代码获取指定区间的日线(未复权)
func (c *Client) DailyByCode(tsCode, startDate, endDate string) ([]model.Bar, error) {
	return fetchRows(c, "daily", barParams("", tsCode, startDate, endDate), dailyFields, parseBar)
}

// IndexDaily 按交易日获取指数日线 (如 000300.SH 沪深300, 000001.SH 上证综指)
func (c *Client) IndexDaily(tradeDate string) ([]model.Bar, error) {
	return fetchRows(c, "index_daily", barParams(tradeDate, "", "", ""), dailyFields, parseBar)
}

// IndexDailyByCode 按代码获取指数日线
func (c *Client) IndexDailyByCode(tsCode, startDate, endDate string) ([]model.Bar, error) {
	return fetchRows(c, "index_daily", barParams("", tsCode, startDate, endDate), dailyFields, parseBar)
}

// AdjFactor 获取复权因子
// 当 tsCode 或 tradeDate 为空时对应参数不传, 由 Tushare 按默认处理
func (c *Client) AdjFactor(tsCode, tradeDate string) ([]AdjFactor, error) {
	fields := []string{"ts_code", "trade_date", "adj_factor"}
	params := map[string]interface{}{}
	setOpt(params, "ts_code", tsCode)
	setOpt(params, "trade_date", tradeDate)
	return fetchRows(c, "adj_factor", params, fields, func(row map[string]interface{}) AdjFactor {
		return AdjFactor{
			TsCode:    fieldStr(row, "ts_code"),
			TradeDate: fieldStr(row, "trade_date"),
			AdjFactor: fieldFloat(row, "adj_factor"),
		}
	})
}

// DailyBasic 按交易日获取全市场每日基本面数据
func (c *Client) DailyBasic(tradeDate string) ([]model.DailyBasic, error) {
	fields := []string{
		"ts_code", "trade_date", "close", "turnover_rate", "volume_ratio",
		"pe", "pe_ttm", "pb", "ps", "ps_ttm", "dv_ratio",
		"total_mv", "circ_mv", "limit_status",
	}
	return fetchRows(c, "daily_basic", barParams(tradeDate, "", "", ""), fields, func(row map[string]interface{}) model.DailyBasic {
		return model.DailyBasic{
			TsCode:       fieldStr(row, "ts_code"),
			TradeDate:    fieldStr(row, "trade_date"),
			Close:        fieldFloat(row, "close"),
			TurnoverRate: fieldFloat(row, "turnover_rate"),
			VolumeRatio:  fieldFloat(row, "volume_ratio"),
			PE:           fieldFloat(row, "pe"),
			PE_TTM:       fieldFloat(row, "pe_ttm"),
			PB:           fieldFloat(row, "pb"),
			PS:           fieldFloat(row, "ps"),
			PS_TTM:       fieldFloat(row, "ps_ttm"),
			DV_RATIO:     fieldFloat(row, "dv_ratio"),
			TotalMV:      fieldFloat(row, "total_mv"),
			CircMV:       fieldFloat(row, "circ_mv"),
			LimitStatus:  fieldInt(row, "limit_status"),
		}
	})
}

// StkLimit 按交易日获取全市场涨跌停价
func (c *Client) StkLimit(tradeDate string) ([]model.StkLimit, error) {
	fields := []string{"ts_code", "trade_date", "up_limit", "down_limit"}
	return fetchRows(c, "stk_limit", barParams(tradeDate, "", "", ""), fields, func(row map[string]interface{}) model.StkLimit {
		return model.StkLimit{
			TsCode:    fieldStr(row, "ts_code"),
			TradeDate: fieldStr(row, "trade_date"),
			UpLimit:   fieldFloat(row, "up_limit"),
			DownLimit: fieldFloat(row, "down_limit"),
		}
	})
}

// FinaIndicator 获取财务指标
// tsCode: 股票代码, 为空时按报告期获取全市场数据; period: 报告期(如 "20231231"), 为空则返回全部报告期
// 两种典型用法:
//   - 按股票获取: FinaIndicator("000001.SZ", "") 获取该股票全部报告期
//   - 按报告期获取: FinaIndicator("", "20231231") 获取该报告期全市场数据(更高效, 适合批量同步)
func (c *Client) FinaIndicator(tsCode string, period string) ([]model.FinaIndicator, error) {
	fields := []string{
		"ts_code", "ann_date", "end_date", "eps", "roe",
		"grossprofit_margin", "netprofit_margin", "debt_to_assets",
		"netprofit_yoy", "or_yoy", "bps",
	}
	// ts_code 和 period 均为可选参数, 为空时不传, 由 Tushare 按默认处理
	params := map[string]interface{}{}
	setOpt(params, "ts_code", tsCode)
	setOpt(params, "period", period)
	return fetchRows(c, "fina_indicator", params, fields, func(row map[string]interface{}) model.FinaIndicator {
		return model.FinaIndicator{
			TsCode:            fieldStr(row, "ts_code"),
			AnnDate:           fieldStr(row, "ann_date"),
			EndDate:           fieldStr(row, "end_date"),
			EPS:               fieldFloat(row, "eps"),
			ROE:               fieldFloat(row, "roe"),
			GrossProfitMargin: fieldFloat(row, "grossprofit_margin"),
			NetProfitMargin:   fieldFloat(row, "netprofit_margin"),
			DebtToAssets:      fieldFloat(row, "debt_to_assets"),
			NetProfitYoy:      fieldFloat(row, "netprofit_yoy"),
			ORYoy:             fieldFloat(row, "or_yoy"),
			BPS:               fieldFloat(row, "bps"),
		}
	})
}

// NewShare 获取新股申购数据
// startDate/endDate 格式: YYYYMMDD, 为空则返回全部
func (c *Client) NewShare(startDate, endDate string) ([]model.NewShare, error) {
	fields := []string{
		"ts_code", "sub_code", "name", "ipo_date", "issue_date",
		"amount", "market_amount", "price", "pe", "limit_amount", "funds", "ballot",
	}
	params := map[string]interface{}{}
	setOpt(params, "start_date", startDate)
	setOpt(params, "end_date", endDate)
	return fetchRows(c, "new_share", params, fields, func(row map[string]interface{}) model.NewShare {
		return model.NewShare{
			TsCode:       fieldStr(row, "ts_code"),
			SubCode:      fieldStr(row, "sub_code"),
			Name:         fieldStr(row, "name"),
			IpoDate:      fieldStr(row, "ipo_date"),
			IssueDate:    fieldStr(row, "issue_date"),
			Amount:       fieldFloat(row, "amount"),
			MarketAmount: fieldFloat(row, "market_amount"),
			Price:        fieldFloat(row, "price"),
			PE:           fieldFloat(row, "pe"),
			LimitAmount:  fieldFloat(row, "limit_amount"),
			Funds:        fieldFloat(row, "funds"),
			Ballot:       fieldFloat(row, "ballot"),
		}
	})
}

// MajorNews 获取新闻快讯
// startDate/endDate 格式: YYYYMMDD, src 为新闻来源, 空则返回全部
func (c *Client) MajorNews(startDate, endDate string, src string) ([]model.News, error) {
	fields := []string{"datetime", "content", "title", "channels"}
	params := map[string]interface{}{}
	setOpt(params, "start_date", startDate)
	setOpt(params, "end_date", endDate)
	setOpt(params, "src", src)
	return fetchRows(c, "major_news", params, fields, func(row map[string]interface{}) model.News {
		return model.News{
			Datetime: fieldStr(row, "datetime"),
			Content:  fieldStr(row, "content"),
			Title:    fieldStr(row, "title"),
			Channels: fieldStr(row, "channels"),
		}
	})
}

// MoneyFlow 获取个股资金流向(按交易日)
// tradeDate 格式: YYYYMMDD
func (c *Client) MoneyFlow(tradeDate string) ([]model.MoneyFlow, error) {
	fields := []string{
		"ts_code", "trade_date", "buy_elg_amount", "sell_elg_amount", "net_mf_amount",
	}
	return fetchRows(c, "moneyflow", barParams(tradeDate, "", "", ""), fields, func(row map[string]interface{}) model.MoneyFlow {
		return model.MoneyFlow{
			TsCode:        fieldStr(row, "ts_code"),
			TradeDate:     fieldStr(row, "trade_date"),
			BuyElgAmount:  fieldFloat(row, "buy_elg_amount"),
			SellElgAmount: fieldFloat(row, "sell_elg_amount"),
			NetMFAmount:   fieldFloat(row, "net_mf_amount"),
		}
	})
}

// TopList 获取龙虎榜数据(按交易日)
// tradeDate 格式: YYYYMMDD
func (c *Client) TopList(tradeDate string) ([]model.TopList, error) {
	fields := []string{
		"ts_code", "trade_date", "name", "close", "pct_change",
		"turnover_rate", "amount", "net_amount", "buy_amount", "sell_amount",
	}
	return fetchRows(c, "top_list", barParams(tradeDate, "", "", ""), fields, func(row map[string]interface{}) model.TopList {
		return model.TopList{
			TsCode:       fieldStr(row, "ts_code"),
			TradeDate:    fieldStr(row, "trade_date"),
			Name:         fieldStr(row, "name"),
			Close:        fieldFloat(row, "close"),
			PctChange:    fieldFloat(row, "pct_change"),
			TurnoverRate: fieldFloat(row, "turnover_rate"),
			Amount:       fieldFloat(row, "amount"),
			NetAmount:    fieldFloat(row, "net_amount"),
			BuyAmount:    fieldFloat(row, "buy_amount"),
			SellAmount:   fieldFloat(row, "sell_amount"),
		}
	})
}
