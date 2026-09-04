package model

// ===================== 交易域模型 =====================

// OrderTicket 指令单：人在回路的唯一载体，回执的唯一锚点（D3）。
//
// 每列都要点得出读者，算得出的不落：
//   - 止损价、仓位占比 —— 风控参数按当时档位现算，单据不复述一遍算错的旧值；
//   - 来源（哪条规则提的单）—— 写在 reason 文本里；
//   - 创建/更新时间 —— 状态流转写服务日志；
//   - 成交金额 = fill_qty × fill_price，三项费用 = 金额 × 费率配置；
//     只有含费合计 TotalCost 是"这笔实际占用/到账多少现金"的结果，现金推算按它求和。
//
// 一单最多一回执（原 fill.ticket_id 的 UNIQUE 约束即为此），故成交列直接落在本行，
// 未成交时取零值，以 ReportedAt 非空判定"已有回执"。
type OrderTicket struct {
	ID        int64     `db:"id"`
	TradeDate string    `db:"trade_date"`
	TsCode    string    `db:"ts_code"`
	Name      string    `db:"name"` // 开单时的名称快照：股票会改名戴帽，历史单据不能跟着变
	Direction Direction `db:"direction"`
	Qty       Qty       `db:"qty"` // 计划数量
	RefPrice  Fen       `db:"ref_price"`
	Reason    string    `db:"reason"`

	Status     TicketStatus `db:"status"`
	ValidUntil string       `db:"valid_until"` // RFC3339（Asia/Shanghai，带 +08:00 偏移）
	Gear       Gear         `db:"gear"`        // 开单时的档位

	// 成交回执
	FillQty    Qty    `db:"fill_qty"`
	FillPrice  Fen    `db:"fill_price"`
	TotalCost  Fen    `db:"total_cost"` // 含费合计：买入=金额+费，卖出=金额−费
	ReportedBy string `db:"reported_by"`
	ReportedAt string `db:"reported_at"`

	// Note 人工备注：作废原因（skip）与成交备注（report_fill）共用这一列。
	Note string `db:"note"`
}

// HasFill 是否已登记成交回执（原"按 ticket_id 查 fill 表"的同义判定）。
func (o *OrderTicket) HasFill() bool { return o.ReportedAt != "" }

// FillAmount 成交金额（不含费）：由数量与价格现算，不落列。
func (o *OrderTicket) FillAmount() Fen { return o.FillPrice.Mul(o.FillQty) }

// IsActive 是否处于活跃状态（drafted/issued）。
func (o *OrderTicket) IsActive() bool {
	return o.Status == TicketDrafted || o.Status == TicketIssued
}

// Fill 成交回执视图（由指令单行还原，已无独立的成交表）。
type Fill struct {
	TicketID   int64
	TsCode     string
	Direction  Direction
	Qty        Qty
	Price      Fen
	Amount     Fen
	TotalCost  Fen
	TradeDate  string
	ReportedBy string
	ReportedAt string
	Note       string
}

// FillView 从指令单行还原成交回执视图。
func (o *OrderTicket) FillView() Fill {
	return Fill{
		TicketID: o.ID, TsCode: o.TsCode, Direction: o.Direction,
		Qty: o.FillQty, Price: o.FillPrice, Amount: o.FillAmount(), TotalCost: o.TotalCost,
		TradeDate: o.TradeDate, ReportedBy: o.ReportedBy, ReportedAt: o.ReportedAt,
		Note: o.Note,
	}
}

// Position 持仓：每只票一行的当前状态（市值等派生值实时算，不落库）。
type Position struct {
	TsCode        string `db:"ts_code"`
	TotalQty      Qty    `db:"total_qty"`
	TodayBought   Qty    `db:"today_bought"`
	CostPrice     Fen    `db:"cost_price"`
	HighPrice     Fen    `db:"high_price"`
	FirstOpenDate string `db:"first_open_date"`
}

// Available 可卖数量（T+1：今日买入不可卖）。
//
// 不落 available_qty 列：买入时 total 与 today 同增、卖出时同减、隔日结算把 today 归零，
// 任何时刻都恒等于 total_qty − today_bought。存两份就要处理"券商同步进来的两个数不一致"，
// 而那份不一致没有任何读者。
func (p *Position) Available() Qty {
	a := p.TotalQty.Sub(p.TodayBought)
	if a < 0 {
		return 0
	}
	return a
}

// Assets 账户资产：现金 + 持仓市值，全部由成交历史与持仓实时推算得出，不落库。
// TradeDate 是市值取价的截止日，用于判断数据是否陈旧。
type Assets struct {
	TradeDate     string
	Cash          Fen
	MarketValue   Fen
	TotalAsset    Fen
	PositionCount int
}
