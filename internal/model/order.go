package model

import "time"

// Side 买卖方向
type Side int

const (
	SideBuy  Side = 1
	SideSell Side = -1
)

func (s Side) String() string {
	if s == SideBuy {
		return "buy"
	}
	return "sell"
}

// OrderType 订单类型
type OrderType int

const (
	OrderMarket OrderType = iota // 市价单
	OrderLimit                    // 限价单
)

// OrderStatus 订单状态
type OrderStatus int

const (
	StatusCreated  OrderStatus = iota // 已创建
	StatusSubmitted                   // 已提交
	StatusPartial                     // 部分成交
	StatusFilled                      // 全部成交
	StatusCanceled                    // 已撤单
	StatusRejected                    // 已拒绝
)

func (s OrderStatus) String() string {
	switch s {
	case StatusCreated:
		return "created"
	case StatusSubmitted:
		return "submitted"
	case StatusPartial:
		return "partial"
	case StatusFilled:
		return "filled"
	case StatusCanceled:
		return "canceled"
	case StatusRejected:
		return "rejected"
	default:
		return "unknown"
	}
}

// Order 订单
type Order struct {
	ID         int64      `json:"id"`
	RunID      string     `json:"run_id"`      // 回测/实盘运行批次
	TsCode     string     `json:"ts_code"`
	Side       Side       `json:"side"`
	OrderType  OrderType  `json:"order_type"`
	Price      float64    `json:"price"`       // 限价单价格
	Qty        int        `json:"qty"`         // 委托数量
	FilledQty  int        `json:"filled_qty"`  // 已成交数量
	AvgPrice   float64    `json:"avg_price"`   // 成交均价
	Status     OrderStatus `json:"status"`
	Reason     string     `json:"reason"`     // 下单原因
	CreateTime time.Time  `json:"create_time"`
	UpdateTime time.Time  `json:"update_time"`
}

// Trade 成交记录
type Trade struct {
	ID          int64   `json:"id" db:"id"`
	RunID       string  `json:"run_id" db:"run_id"`
	OrderID     int64   `json:"order_id" db:"order_id"`
	TsCode      string  `json:"ts_code" db:"ts_code"`
	Side        Side    `json:"side" db:"side"`
	Price       float64 `json:"price" db:"price"`
	Qty         int     `json:"qty" db:"qty"`
	Amount      float64 `json:"amount" db:"amount"`           // 成交金额
	Commission  float64 `json:"commission" db:"commission"`   // 佣金
	StampTax    float64 `json:"stamp_tax" db:"stamp_tax"`     // 印花税
	TransferFee float64 `json:"transfer_fee" db:"transfer_fee"` // 过户费
	TotalCost   float64 `json:"total_cost" db:"total_cost"`   // 总费用
	TradeDate   string  `json:"trade_date" db:"trade_date"`
	TradeTime   string  `json:"trade_time" db:"trade_time"`
}
