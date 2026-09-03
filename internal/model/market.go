package model

// ===================== 市场域模型 =====================
// 字段名带后缀表示非"分/股"单位（ARCHITECTURE §3.0）：
//   vol_lot（手）、amount_k（千元）、total_mv_w / circ_mv_w（万元）。
// 价格类字段（open/high/low/close/pre_close/raw_close/up_limit/down_limit）
// 一律为 Fen（分）。

// Bar 日线行情（全链路唯一口径：前复权）。
type Bar struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	Open      Fen     `db:"open"`
	High      Fen     `db:"high"`
	Low       Fen     `db:"low"`
	Close     Fen     `db:"close"`
	PreClose  Fen     `db:"pre_close"`
	PctChg    float64 `db:"pct_chg"`
	VolLot    float64 `db:"vol_lot"`
	AmountK   float64 `db:"amount_k"`
	AdjFactor float64 `db:"adj_factor"`
	RawClose  Fen     `db:"raw_close"` // 未复权收盘，涨跌停/停牌判定用
}

// DailyBasic 每日指标（PE/PB/换手/市值）。
type DailyBasic struct {
	TsCode       string  `db:"ts_code"`
	TradeDate    string  `db:"trade_date"`
	Close        Fen     `db:"close"`
	TurnoverRate float64 `db:"turnover_rate"`
	VolumeRatio  float64 `db:"volume_ratio"`
	PE           float64 `db:"pe"`
	PETtm        float64 `db:"pe_ttm"`
	PB           float64 `db:"pb"`
	PsTtm        float64 `db:"ps_ttm"`
	DvRatio      float64 `db:"dv_ratio"`
	TotalMvW     float64 `db:"total_mv_w"` // 万元
	CircMvW      float64 `db:"circ_mv_w"` // 万元
}

// StockBasic 股票基础信息。
type StockBasic struct {
	TsCode     string `db:"ts_code"`
	Symbol     string `db:"symbol"`
	Name       string `db:"name"`
	Market     string `db:"market"`
	Exchange   string `db:"exchange"`
	Industry   string `db:"industry"`
	ListDate   string `db:"list_date"`
	DelistDate string `db:"delist_date"`
	IsST       bool   `db:"is_st"`
	ListStatus string `db:"list_status"`
	UpdatedAt  string `db:"updated_at"`
}

// PriceLimit 涨跌停价（涨停禁买/跌停禁卖唯一判定依据，不依赖状态编码猜测）。
type PriceLimit struct {
	TsCode    string `db:"ts_code"`
	TradeDate string `db:"trade_date"`
	UpLimit   Fen    `db:"up_limit"`
	DownLimit Fen    `db:"down_limit"`
}

// Suspend 停牌信息。
type Suspend struct {
	TsCode        string `db:"ts_code"`
	TradeDate     string `db:"trade_date"`
	SuspendType   string `db:"suspend_type"`
	SuspendTiming string `db:"suspend_timing"`
}

// FinaIndicator 财务指标（按 ann_date point-in-time，无前视偏差）。
type FinaIndicator struct {
	TsCode           string  `db:"ts_code"`
	EndDate          string  `db:"end_date"`
	AnnDate          string  `db:"ann_date"`
	EPS              float64 `db:"eps"`
	ROE              float64 `db:"roe"`
	ROEDt            float64 `db:"roe_dt"`
	GrossProfitMargin float64 `db:"grossprofit_margin"`
	NetprofitMargin  float64 `db:"netprofit_margin"`
	DebtToAssets     float64 `db:"debt_to_assets"`
	NetprofitYoy     float64 `db:"netprofit_yoy"`
	OrYoy            float64 `db:"or_yoy"`
	BPS              float64 `db:"bps"`
}

// IndexDaily 大盘指数日线（卖出规则"大盘恶化"与 P1-4 数据源）。
type IndexDaily struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	Close     Fen     `db:"close"`
	MA20      float64 `db:"ma20"`
}

// MoneyFlow 个股资金流（P1-5，按日全市场暴增，不建 trade_date 索引）。
type MoneyFlow struct {
	TsCode         string  `db:"ts_code"`
	TradeDate      string  `db:"trade_date"`
	BuyElgAmount   float64 `db:"buy_elg_amount"`
	SellElgAmount  float64 `db:"sell_elg_amount"`
	NetMfAmount    float64 `db:"net_mf_amount"`
}
