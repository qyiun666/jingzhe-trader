package model

// News 新闻快讯
type News struct {
	ID       int64  `json:"id" db:"id"`             // ID
	Datetime string `json:"datetime" db:"datetime"` // 新闻时间
	Content  string `json:"content" db:"content"`   // 内容
	Title    string `json:"title" db:"title"`       // 标题
	Channels string `json:"channels" db:"channels"` // 频道
}

// MoneyFlow 个股资金流向
type MoneyFlow struct {
	TsCode        string  `json:"ts_code" db:"ts_code"`                 // 股票代码
	TradeDate     string  `json:"trade_date" db:"trade_date"`           // 交易日期
	BuyElgAmount  float64 `json:"buy_elg_amount" db:"buy_elg_amount"`   // 超大单买入金额(万元)
	SellElgAmount float64 `json:"sell_elg_amount" db:"sell_elg_amount"` // 超大单卖出金额(万元)
	NetMFAmount   float64 `json:"net_mf_amount" db:"net_mf_amount"`     // 净流入金额(万元)
}

// TopList 龙虎榜
type TopList struct {
	TsCode       string  `json:"ts_code" db:"ts_code"`             // 股票代码
	TradeDate    string  `json:"trade_date" db:"trade_date"`       // 交易日期
	Name         string  `json:"name" db:"name"`                   // 股票名称
	Close        float64 `json:"close" db:"close"`                 // 收盘价
	PctChange    float64 `json:"pct_change" db:"pct_change"`       // 涨跌幅
	TurnoverRate float64 `json:"turnover_rate" db:"turnover_rate"` // 换手率
	Amount       float64 `json:"amount" db:"amount"`               // 成交额(万元)
	NetAmount    float64 `json:"net_amount" db:"net_amount"`       // 龙虎榜净买入额(万元)
	BuyAmount    float64 `json:"buy_amount" db:"buy_amount"`       // 龙虎榜买入额(万元)
	SellAmount   float64 `json:"sell_amount" db:"sell_amount"`     // 龙虎榜卖出额(万元)
}
