package risk

import (
	"math"

	"jingzhe-trader/internal/model"
)

// A 股双边交易成本（与券商/税务口径一致，只用于估算，不替代成交回执）。
//
// 三个费用都按"成交金额"计；佣金每笔有 5 元下限，所以小仓位的实际费率远高于
// 名义费率——1200 元一笔的双边成本约 11.2 元（0.94%），5000 元一笔约 15.1 元（0.30%）。
// 这正是弱势试探档必须提高最小交易额的原因：仓位太小，成本先把收益吃光。
const (
	CommissionRate   = 0.00025            // 佣金万 2.5
	MinCommissionFen = model.Fen(5 * 100) // 每笔最低 5 元
	StampTaxRate     = 0.001              // 印花税千 1，仅卖出
	TransferFeeRate  = 0.00001            // 过户费十万分之一，双边
)

// BuyFeeFen 买入费用（佣金 + 过户费），amount 为成交金额（分）。
func BuyFeeFen(amount model.Fen) model.Fen {
	if amount <= 0 {
		return 0
	}
	return commissionFen(amount) + transferFeeFen(amount)
}

// SellFeeFen 卖出费用（佣金 + 印花税 + 过户费），amount 为成交金额（分）。
func SellFeeFen(amount model.Fen) model.Fen {
	if amount <= 0 {
		return 0
	}
	return commissionFen(amount) + model.Fen(math.Round(float64(amount)*StampTaxRate)) + transferFeeFen(amount)
}

// RoundTripCostFen 双边成本估算：按买入金额 = 卖出金额 = amount 计。
//
// 归因与 prompt 只需要一个量级正确的成本线，不追求与实际成交分毫不差；
// 真实费用仍由成交回执落账。
func RoundTripCostFen(amount model.Fen) model.Fen {
	return BuyFeeFen(amount) + SellFeeFen(amount)
}

// commissionFen 佣金向上取整到分：宁可高估一分，也不让成本线被低估。
func commissionFen(amount model.Fen) model.Fen {
	fee := model.Fen(math.Ceil(float64(amount) * CommissionRate))
	if fee < MinCommissionFen {
		return MinCommissionFen
	}
	return fee
}

func transferFeeFen(amount model.Fen) model.Fen {
	return model.Fen(math.Round(float64(amount) * TransferFeeRate))
}
