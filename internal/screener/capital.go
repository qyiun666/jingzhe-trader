package screener

import "jingzhe-trader/internal/market"

// lotValue 一手对应的股数; 价格带按"整手是否买得起"反推
const lotValue = float64(market.LotSize)

// Capital 选股时的资金快照, 由组合根从账户资产与风控/交易配置算出后注入
// (screener 是能力包, 不得自行构造 broker/config 依赖)
type Capital struct {
	TotalAsset     float64 // 总资产
	Cash           float64 // 可用现金
	MaxPositionPct float64 // 单票仓位占总资产上限 (risk.max_position_pct)
	MinTradeAmount float64 // 最小单笔金额 (trading.min_trade_amount)
}

// CapitalSource 提供实时资金快照
type CapitalSource interface {
	Capital() Capital
}

// PriceBand 按资金反推出的可选价格带 [lo, hi], 0 表示该侧不额外约束
//
// 上限 = 一手买得起的最高价: 单票仓位上限与可用现金取小。
// 股价高于此价的候选必然被风控裁成 0 股, 选出来只是噪音 (小资金尤其明显:
// 1 万元账户下 40 元以上的票连 1 手都够不到 40% 仓位线)。
//
// 下限 = 最小单笔金额对应价: A股 100 股整手, 单价过低的票一手凑不满
// trading.min_trade_amount, 会被风控以"最低佣金侵蚀"为由拒单, 同样不该入选。
func (c Capital) PriceBand() (lo, hi float64) {
	if c.MinTradeAmount > 0 {
		lo = c.MinTradeAmount / lotValue
	}
	if budget := c.buyBudget(); budget > 0 {
		hi = budget / lotValue
	}
	return lo, hi
}

// buyBudget 单笔买入可用的金额上界: 单票仓位上限与可用现金取小 (缺失的一侧不参与比较)
func (c Capital) buyBudget() float64 {
	budget := 0.0
	consider := func(v float64) {
		if v > 0 && (budget <= 0 || v < budget) {
			budget = v
		}
	}
	consider(c.TotalAsset * c.MaxPositionPct)
	consider(c.Cash)
	return budget
}
