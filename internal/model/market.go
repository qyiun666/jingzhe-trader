package model

// ===================== 市场域模型 =====================
// 价格类字段一律为 Fen（分）；vol_lot 单位为手。

// Bar 日线行情（前复权口径）。指数与个股共用本结构，指数的 RawClose/VolLot 为 0。
//
// 三列各有读者：Close 是指标与因子口径（前复权，保证跨除权连续），
// RawClose 是真实成交价口径（下单档位、持仓市值、一手价判定都用它），
// VolLot 是量能口径。开高低走、涨跌幅、成交额全链路无消费者，不落库。
type Bar struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	Close     Fen     `db:"close"`
	VolLot    float64 `db:"vol_lot"`
	RawClose  Fen     `db:"raw_close"`
}

// StockBasic 每只票一行的当前快照：静态属性 + 最近一个交易日的估值截面。
//
// 原 daily_basic 表按 (ts_code, trade_date) 每票每日一行，但全项目只有一个读者
// "选股当日整批读一次"，没有任何历史复算路径读旧日期 —— 那它就是"这只票现在的估值"，
// 与名称/行业/上市日同粒度，合一张表。
//
// ValDate 标注估值来自哪个交易日：选股据此核对"别拿今天的 PE 去补跑上周的仓"。
// 是否 ST 不落列：名称就是真相源（market.IsSTName），戴帽摘帽先反映在名称上。
type StockBasic struct {
	TsCode       string  `db:"ts_code"`
	Name         string  `db:"name"`
	Industry     string  `db:"industry"`
	ListDate     string  `db:"list_date"`
	ListStatus   string  `db:"list_status"`
	ValDate      string  `db:"val_date"`
	TurnoverRate float64 `db:"turnover_rate"`
	PETtm        float64 `db:"pe_ttm"`
	PB           float64 `db:"pb"`
	CircMvW      float64 `db:"circ_mv_w"` // 万元
}

// Valuation 每日指标接口的一行返回：只含估值四项，落到 stock_basic 的估值列。
//
// 不含 Close —— 一手价用 daily_bar.raw_close（同一天的真实价），两处各存一份必然漂。
// 不带 TradeDate：截面属于哪一天由同步侧的入参决定，写进 val_date 列。
type Valuation struct {
	TsCode       string  `db:"ts_code"`
	TurnoverRate float64 `db:"turnover_rate"`
	PETtm        float64 `db:"pe_ttm"`
	PB           float64 `db:"pb"`
	CircMvW      float64 `db:"circ_mv_w"` // 万元
}
