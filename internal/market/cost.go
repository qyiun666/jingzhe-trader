package market

import (
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
)

// CostModel 交易费用模型
//
// A 股交易费用构成:
//   - 佣金:   双向收取, 按成交金额 × 佣金费率计算, 不低于单笔最低佣金
//   - 印花税: 仅卖出收取, 按成交金额 × 印花税率计算
//   - 过户费: 双向收取, 按成交金额 × 过户费率计算
type CostModel struct {
	CommissionRate  float64 // 佣金费率
	MinCommission   float64 // 单笔最低佣金
	StampTaxRate    float64 // 印花税率 (仅卖出)
	TransferFeeRate float64 // 过户费率 (双向)
}

// NewCostModel 从配置创建交易费用模型
func NewCostModel(cfg config.CostConfig) *CostModel {
	return &CostModel{
		CommissionRate:  cfg.CommissionRate,
		MinCommission:   cfg.MinCommission,
		StampTaxRate:    cfg.StampTaxRate,
		TransferFeeRate: cfg.TransferFeeRate,
	}
}

// Calculate 计算交易费用
//   - 佣金:   max(amount × CommissionRate, MinCommission)
//   - 印花税: 仅卖出, amount × StampTaxRate
//   - 过户费: 双向, amount × TransferFeeRate
//
// amount = price × qty
func (cm *CostModel) Calculate(side model.Side, price float64, qty int) model.Cost {
	if qty <= 0 || price <= 0 {
		return model.Cost{}
	}
	amount := price * float64(qty)

	// 佣金: 双向, 不低于最低佣金
	commission := amount * cm.CommissionRate
	if commission < cm.MinCommission {
		commission = cm.MinCommission
	}

	// 印花税: 仅卖出
	stampTax := 0.0
	if side == model.SideSell {
		stampTax = amount * cm.StampTaxRate
	}

	// 过户费: 双向
	transferFee := amount * cm.TransferFeeRate

	return model.Cost{
		Commission:  commission,
		StampTax:    stampTax,
		TransferFee: transferFee,
	}
}

// BuyCost 计算买入总花费 (含费用)
// 买入花费 = 成交金额 + 佣金 + 过户费 (买入无印花税)
func (cm *CostModel) BuyCost(price float64, qty int) float64 {
	if qty <= 0 || price <= 0 {
		return 0
	}
	amount := price * float64(qty)
	cost := cm.Calculate(model.SideBuy, price, qty)
	return amount + cost.Commission + cost.TransferFee
}

// SellIncome 计算卖出净收入 (扣费用)
// 卖出收入 = 成交金额 - 佣金 - 印花税 - 过户费
func (cm *CostModel) SellIncome(price float64, qty int) float64 {
	if qty <= 0 || price <= 0 {
		return 0
	}
	amount := price * float64(qty)
	cost := cm.Calculate(model.SideSell, price, qty)
	return amount - cost.Total()
}
