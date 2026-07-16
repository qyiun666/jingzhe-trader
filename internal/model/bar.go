package model

import "time"

// Bar K线数据
type Bar struct {
	TsCode    string  `json:"ts_code" db:"ts_code"`    // 股票代码 000001.SZ
	TradeDate string  `json:"trade_date" db:"trade_date"` // 交易日期 YYYYMMDD
	Open      float64 `json:"open" db:"open"`
	High      float64 `json:"high" db:"high"`
	Low       float64 `json:"low" db:"low"`
	Close     float64 `json:"close" db:"close"`
	PreClose  float64 `json:"pre_close" db:"pre_close"` // 昨收价
	Change    float64 `json:"change" db:"change"`       // 涨跌额
	PctChg    float64 `json:"pct_chg" db:"pct_chg"`     // 涨跌幅 %
	Vol       float64 `json:"vol" db:"vol"`             // 成交量 (手)
	Amount    float64 `json:"amount" db:"amount"`       // 成交额 (千元)
	AdjFactor float64 `json:"adj_factor" db:"adj_factor"` // 复权因子
}

// TradeDate 转换为 time.Time
func (b *Bar) Date() time.Time {
	t, _ := time.Parse("20060102", b.TradeDate)
	return t
}

// AdjOpen 前复权开盘价
func (b *Bar) AdjOpen() float64 {
	if b.AdjFactor > 0 {
		return b.Open * b.AdjFactor
	}
	return b.Open
}

// AdjClose 前复权收盘价
func (b *Bar) AdjClose() float64 {
	if b.AdjFactor > 0 {
		return b.Close * b.AdjFactor
	}
	return b.Close
}

// AdjHigh 前复权最高价
func (b *Bar) AdjHigh() float64 {
	if b.AdjFactor > 0 {
		return b.High * b.AdjFactor
	}
	return b.High
}

// AdjLow 前复权最低价
func (b *Bar) AdjLow() float64 {
	if b.AdjFactor > 0 {
		return b.Low * b.AdjFactor
	}
	return b.Low
}
