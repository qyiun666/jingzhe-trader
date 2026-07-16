package model

// DailyBasic 每日基本面数据
type DailyBasic struct {
	TsCode       string  `json:"ts_code" db:"ts_code"`
	TradeDate    string  `json:"trade_date" db:"trade_date"`
	Close        float64 `json:"close" db:"close"`
	TurnoverRate float64 `json:"turnover_rate" db:"turnover_rate"` // 换手率 %
	VolumeRatio  float64 `json:"volume_ratio" db:"volume_ratio"`   // 量比
	PE           float64 `json:"pe" db:"pe"`                       // 市盈率
	PE_TTM       float64 `json:"pe_ttm" db:"pe_ttm"`               // 市盈率TTM
	PB           float64 `json:"pb" db:"pb"`                       // 市净率
	PS           float64 `json:"ps" db:"ps"`                       // 市销率
	PS_TTM       float64 `json:"ps_ttm" db:"ps_ttm"`               // 市销率TTM
	DV_RATIO     float64 `json:"dv_ratio" db:"dv_ratio"`           // 股息率
	TotalMV      float64 `json:"total_mv" db:"total_mv"`           // 总市值 (万元)
	CircMV       float64 `json:"circ_mv" db:"circ_mv"`             // 流通市值 (万元)
	LimitStatus  int     `json:"limit_status" db:"limit_status"`   // 涨跌停状态: 1涨停 -1跌停 0正常
}

// FinaIndicator 财务指标
type FinaIndicator struct {
	TsCode            string  `json:"ts_code" db:"ts_code"`
	AnnDate           string  `json:"ann_date" db:"ann_date"`                     // 公告日期
	EndDate           string  `json:"end_date" db:"end_date"`                     // 报告期
	EPS               float64 `json:"eps" db:"eps"`                               // 每股收益
	ROE               float64 `json:"roe" db:"roe"`                               // 净资产收益率
	GrossProfitMargin float64 `json:"grossprofit_margin" db:"grossprofit_margin"` // 毛利率
	NetProfitMargin   float64 `json:"netprofit_margin" db:"netprofit_margin"`     // 净利率
	DebtToAssets      float64 `json:"debt_to_assets" db:"debt_to_assets"`         // 资产负债率
	NetProfitYoy      float64 `json:"netprofit_yoy" db:"netprofit_yoy"`           // 净利润同比 %
	ORYoy             float64 `json:"or_yoy" db:"or_yoy"`                         // 营收同比 %
	BPS               float64 `json:"bps" db:"bps"`                               // 每股净资产
}

// StkLimit 涨跌停价
type StkLimit struct {
	TsCode    string  `json:"ts_code" db:"ts_code"`
	TradeDate string  `json:"trade_date" db:"trade_date"`
	UpLimit   float64 `json:"up_limit" db:"up_limit"`     // 涨停价
	DownLimit float64 `json:"down_limit" db:"down_limit"` // 跌停价
}

// TradeCal 交易日历
type TradeCal struct {
	CalDate      string `json:"cal_date" db:"cal_date"`           // 日历日期 YYYYMMDD
	IsOpen       int    `json:"is_open" db:"is_open"`             // 0休市 1交易
	PreTradeDate string `json:"pretrade_date" db:"pretrade_date"` // 上一交易日
	PExchange    string `json:"exchange" db:"exchange"`           // 交易所 SSE/SZSE
}
