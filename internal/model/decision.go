package model

// ===================== 决策链内存模型 =====================
//
// 本文件的类型只在一次运行内传递（选股 → 决策 → 指令单），一律不落库：
// 落库的结果只有 order_ticket（含成交列）与 position，过程只写日志。

// FactorScore 因子分项得分（可解释，0-100 截面百分位）。
type FactorScore struct {
	Momentum  float64
	Value     float64
	LowVol    float64
	Liquidity float64
}

// Signal 一条买卖决策：买入来自 LLM 评审，卖出来自持仓规则，一律交给指令单落地。
type Signal struct {
	TradeDate  string
	TsCode     string
	Name       string
	Direction  Direction
	Rule       string // 决策来源（买入 llm_review / 卖出为止损止盈等规则名）
	Confidence float64
	RefPrice   Fen // 分
	Reason     string
}

// 品种类别：选股漏斗与决策链按它区分个股与场内 ETF。
//
// 必须是候选身上的显式旗标而不是"行业字段等于某个值"这类隐式约定：两者的可得数据
// 完全不同（ETF 没有 PE/PB/换手率/流通市值），prompt 与告警文案按同一套字段渲染
// 就会把"没有这个数据"讲成"这个数据是 0"——PE 0.0 在模型眼里是极其便宜，不是缺数据。
const (
	// KindStock 个股。Candidate 的零值按个股处理，老路径不必显式赋值。
	KindStock = "stock"
	KindETF   = "etf"
)

// Candidate 选股候选：漏斗与 LLM 决策与指令单之间传递的唯一对象。
// Close 取未复权收盘（与成交成本同口径），Mom 为因子窗口区间涨幅（小数）。
type Candidate struct {
	Rank         int
	PoolSize     int    // 因子百分位的截面基数；基数很小时 0/100 只表示相对位置，不是绝对评价
	Kind         string // KindStock（零值）/ KindETF
	TsCode       string
	Name         string
	Industry     string
	Score        float64
	Factors      FactorScore
	Close        Fen
	CircMvW      float64
	PETtm        float64
	PB           float64
	TurnoverRate float64
	Mom          float64
	SectorMom    float64
	Reason       string
}

// IsETF 是否场内 ETF：下游（LLM prompt、告警文案）按它换一套字段渲染。
func (c Candidate) IsETF() bool { return c.Kind == KindETF }

// SectorStat 板块强弱统计（只出现在日志与告警正文）。
type SectorStat struct {
	Industry string
	Members  int
	Scorable int     // 窗口内可算动量的成员数
	WMom     float64 // 流通市值加权区间涨幅（小数）
	Retained bool    // 是否进入 Top K 强势板块
}
