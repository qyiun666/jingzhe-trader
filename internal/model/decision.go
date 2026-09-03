package model

// ===================== 选股与信号域模型 =====================

// Signal 信号：买入/卖出规则触发，含可解释理由与风控否决留痕（D1）。
type Signal struct {
	ID         int64     `db:"id"`
	TradeDate  string    `db:"trade_date"`
	TsCode     string    `db:"ts_code"`
	Name       string    `db:"name"`
	Direction  Direction `db:"direction"`
	Rule       string    `db:"rule"`     // 触发规则名
	Confidence float64   `db:"confidence"`
	RefPrice   Fen       `db:"ref_price"` // 分
	Reason     string    `db:"reason"`
	Payload    string    `db:"payload"`    // JSON：关键数值快照
	Status     string    `db:"status"`     // new|passed|rejected|converted
	RejectRule string    `db:"reject_rule"` // 被风控否决时的规则名
	RejectMsg  string    `db:"reject_msg"`
	CreatedAt  string    `db:"created_at"`
}

// FactorScore 五因子分项得分（可解释）。
type FactorScore struct {
	Momentum  float64 `db:"f_momentum"`
	Quality   float64 `db:"f_quality"`
	Value     float64 `db:"f_value"`
	LowVol    float64 `db:"f_lowvol"`
	Liquidity float64 `db:"f_liquidity"`
}

// ScreenResult 选股结果：综合排名 + 五因子分项 + 可解释理由。
type ScreenResult struct {
	TradeDate     string      `db:"trade_date"`
	TsCode        string      `db:"ts_code"`
	Rank          int         `db:"rank"`
	Score         float64     `db:"score"`
	Factors       FactorScore `db:"-"`
	F_Momentum    float64     `db:"f_momentum"`
	F_Quality     float64     `db:"f_quality"`
	F_Value       float64     `db:"f_value"`
	F_LowVol      float64     `db:"f_lowvol"`
	F_Liquidity   float64     `db:"f_liquidity"`
	Close         Fen         `db:"close"`
	CircMvW       float64     `db:"circ_mv_w"`
	PETtm         float64     `db:"pe_ttm"`
	PB            float64     `db:"pb"`
	TurnoverRate  float64     `db:"turnover_rate"`
	Reason        string      `db:"reason"`
}
