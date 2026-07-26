package risk

// SizeLimits 小资金自适应资金管理限制
// 用于摊薄 A 股最低佣金(如5元)对小资金的费率侵蚀, 并控制持仓分散度
type SizeLimits struct {
	MinTradeAmount float64 // 最小单笔交易金额, 0=按最低佣金自适应
	MaxPositions   int     // 最大持仓数, 0=按资金规模自适应
	MinCommission  float64 // 单笔最低佣金 (自适应计算依据)
}

// ResolveMinAmount 解析最小单笔交易金额
// 自适应规则: 最低佣金 / 0.1% (如 5元/0.001 = 5000元), 保证单边佣金费率不超过0.1%
func (s SizeLimits) ResolveMinAmount() float64 {
	if s.MinTradeAmount > 0 {
		return s.MinTradeAmount
	}
	if s.MinCommission > 0 {
		return s.MinCommission / 0.001
	}
	return 0
}

// ResolveMaxPositions 按资金规模解析最大持仓数
// <5万 → 2只(集中持仓摊薄费用), <20万 → 4只, 否则 6只
func (s SizeLimits) ResolveMaxPositions(capital float64) int {
	if s.MaxPositions > 0 {
		return s.MaxPositions
	}
	switch {
	case capital < 50000:
		return 2
	case capital < 200000:
		return 4
	default:
		return 6
	}
}
