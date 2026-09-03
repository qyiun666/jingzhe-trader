package model

import "time"

// ===================== 交易域模型 =====================

// OrderTicket 指令单：人在回路的唯一载体，回执的唯一锚点（D3）。
type OrderTicket struct {
	ID           int64         `db:"id"`
	TradeDate    string        `db:"trade_date"`
	TsCode       string        `db:"ts_code"`
	Name         string        `db:"name"`
	Direction    Direction     `db:"direction"`
	Qty          Qty           `db:"qty"`
	RefPriceLow  Fen           `db:"ref_price_low"`
	RefPriceHigh Fen           `db:"ref_price_high"`
	StopPrice    Fen           `db:"stop_price"`
	Reason       string        `db:"reason"`
	PositionPct  float64       `db:"position_pct"`
	Urgency      string        `db:"urgency"`
	Source       string        `db:"source"`
	Status       TicketStatus  `db:"status"`
	ValidUntil   string        `db:"valid_until"` // RFC3339（Asia/Shanghai，带 +08:00 偏移）
	Gear         Gear          `db:"gear"`
	ProfitLock   bool          `db:"profit_lock"`
	GoalSnapshot string        `db:"goal_snapshot"`
	SignalID     int64         `db:"signal_id"`
	SkipReason   string        `db:"skip_reason"`
	CreatedAt    string        `db:"created_at"`
	UpdatedAt    string        `db:"updated_at"`
	IssuedAt     string        `db:"issued_at"`
	ClosedAt     string        `db:"closed_at"`
}

// IsActive 是否处于活跃状态（drafted/issued）。
func (o *OrderTicket) IsActive() bool {
	return o.Status == TicketDrafted || o.Status == TicketIssued
}

// IsExpired 是否已过有效期（与 now 比较；ValidUntil 为带偏移的 RFC3339）。
func (o *OrderTicket) IsExpired(now time.Time) bool {
	if o.ValidUntil == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, o.ValidUntil)
	if err != nil {
		return false
	}
	return now.After(t)
}

// Fill 成交记录：ticket_id 唯一 → 回执天然幂等。
type Fill struct {
	ID          int64   `db:"id"`
	TicketID    int64   `db:"ticket_id"`
	TsCode      string  `db:"ts_code"`
	Direction   Direction `db:"direction"`
	Qty         Qty     `db:"qty"`
	Price       Fen     `db:"price"`
	Amount      Fen     `db:"amount"`
	Commission  Fen     `db:"commission"`
	StampTax    Fen     `db:"stamp_tax"`
	TransferFee Fen     `db:"transfer_fee"`
	TotalCost   Fen     `db:"total_cost"`
	TradeDate   string  `db:"trade_date"`
	ReportedBy  string  `db:"reported_by"`
	ReportedAt  string  `db:"reported_at"`
	Note        string  `db:"note"`
}

// Position 持仓（市值派生值不落库，实时计算）。
type Position struct {
	TsCode       string `db:"ts_code"`
	TotalQty     Qty    `db:"total_qty"`
	AvailableQty Qty    `db:"available_qty"`
	TodayBought  Qty    `db:"today_bought"`
	CostPrice    Fen    `db:"cost_price"`
	HighPrice    Fen    `db:"high_price"`
	FirstOpenDate string `db:"first_open_date"`
	UpdatedAt    string `db:"updated_at"`
}

// AccountSnapshot 账户快照：季度目标的数据源（日终派生市值落快照）。
type AccountSnapshot struct {
	TradeDate     string `db:"trade_date"`
	Cash          Fen    `db:"cash"`
	MarketValue   Fen    `db:"market_value"`
	TotalAsset    Fen    `db:"total_asset"`
	PositionCount int    `db:"position_count"`
	Gear          Gear   `db:"gear"`
	ProfitLock    bool   `db:"profit_lock"`
	CreatedAt     string `db:"created_at"`
}
