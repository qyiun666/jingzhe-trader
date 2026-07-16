package model

// Position 持仓 (支持T+1)
type Position struct {
	TsCode         string  `json:"ts_code" db:"ts_code"`
	TotalQty       int     `json:"total_qty" db:"total_qty"`                   // 总持仓量
	AvailableQty   int     `json:"available_qty" db:"available_qty"`           // 可卖量 (T+1: 今日买入不计入)
	TodayBought    int     `json:"today_bought" db:"-"`                        // 今日买入量 (次日转入available)
	CostPrice      float64 `json:"cost_price" db:"cost_price"`                 // 持仓成本价
	MarketPrice    float64 `json:"market_price" db:"market_price"`             // 最新市价
	MarketValue    float64 `json:"market_value" db:"market_value"`             // 持仓市值
	FloatingPnL    float64 `json:"floating_pnl" db:"floating_pnl"`             // 浮动盈亏
	FloatingPnLPct float64 `json:"floating_pnl_pct" db:"-"`                    // 浮动盈亏比例
}

// Cost 交易费用
type Cost struct {
	Commission  float64 // 佣金
	StampTax    float64 // 印花税 (仅卖出)
	TransferFee float64 // 过户费 (双向)
}

// Total 总费用
func (c Cost) Total() float64 {
	return c.Commission + c.StampTax + c.TransferFee
}

// AccountSnapshot 账户快照
type AccountSnapshot struct {
	TradeDate   string  `json:"trade_date" db:"trade_date"`
	TotalAsset  float64 `json:"total_asset" db:"total_asset"`   // 总资产
	Cash        float64 `json:"cash" db:"cash"`                 // 可用现金
	MarketValue float64 `json:"market_value" db:"market_value"` // 持仓市值
	PnL         float64 `json:"pnl" db:"pnl"`                   // 当日盈亏
	PnLPct      float64 `json:"pnl_pct" db:"pnl_pct"`           // 当日盈亏比例
	TotalPnL    float64 `json:"total_pnl" db:"total_pnl"`       // 累计盈亏
	TotalPnLPct float64 `json:"total_pnl_pct" db:"total_pnl_pct"` // 累计盈亏比例
}

// Signal 交易信号
type Signal struct {
	TsCode    string  `json:"ts_code"`
	Direction int     `json:"direction"`  // 1买入 -1卖出 0持有
	TargetQty int     `json:"target_qty"` // 目标数量
	Reason    string  `json:"reason"`     // 信号原因
	Strength  float64 `json:"strength"`   // 信号强度 0~1
}

// Direction 常量
const (
	DirBuy   = 1
	DirSell  = -1
	DirHold  = 0
)
