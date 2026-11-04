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
//
// 两套费用档位（20261104 策略 B3）：
//   - 个股：佣金 + 过户费 + 卖出印花税；
//   - 场内基金（ETF）：佣金 + 过户费，**卖出无印花税**（证券交易印花税只对股票及以股票
//     为标的的凭证征收，基金份额转让不在征收范围）。
//
// 为什么必须分档而不是"给 ETF 也按个股算"：印花税千一在 1,100 元级名义上是 1.1 元，
// 占该笔双边成本的约 10%，而 FrictionPct 同时是摩擦闸的判据和移动止盈武装线的输入
// （见 TrailingArmPctFor）——用错档位会让 ETF 的武装线系统性偏高约 0.11pp，止盈比
// 必要的更晚触发，归因的净收益也一并低估。
//
// 个股档保持原有导出面与数值逐字不变：ETF 档是新增分支，不改任何已被测试锁死的口径。
const (
	CommissionRate   = 0.00025            // 佣金万 2.5
	MinCommissionFen = model.Fen(5 * 100) // 每笔最低 5 元
	StampTaxRate     = 0.001              // 印花税千 1，仅卖出、仅个股
	TransferFeeRate  = 0.00001            // 过户费十万分之一，双边
)

// BuyFeeFen 买入费用（佣金 + 过户费），amount 为成交金额（分）。
//
// 买入侧本来就没有印花税，所以个股与场内基金同值，不必分档。
func BuyFeeFen(amount model.Fen) model.Fen {
	if amount <= 0 {
		return 0
	}
	return commissionFen(amount) + transferFeeFen(amount)
}

// SellFeeFen 卖出费用（佣金 + 印花税 + 过户费），amount 为成交金额（分）。个股档。
func SellFeeFen(amount model.Fen) model.Fen {
	if amount <= 0 {
		return 0
	}
	return sellFeeFen(amount, true)
}

// SellFeeFundFen 卖出费用（场内基金档）：佣金 + 过户费，**不含印花税**。
func SellFeeFundFen(amount model.Fen) model.Fen {
	if amount <= 0 {
		return 0
	}
	return sellFeeFen(amount, false)
}

// sellFeeFen 卖出费用的唯一实现；stamp=false 即场内基金档。
func sellFeeFen(amount model.Fen, stamp bool) model.Fen {
	fee := commissionFen(amount) + transferFeeFen(amount)
	if stamp {
		fee += model.Fen(math.Round(float64(amount) * StampTaxRate))
	}
	return fee
}

// SellFeeFenFor 按标的品种取卖出费用：isFund=true 走场内基金档（免印花税）。
func SellFeeFenFor(amount model.Fen, isFund bool) model.Fen {
	if amount <= 0 {
		return 0
	}
	return sellFeeFen(amount, !isFund)
}

// RoundTripCostFen 双边成本估算（个股档）：按买入金额 = 卖出金额 = amount 计。
//
// 归因与 prompt 只需要一个量级正确的成本线，不追求与实际成交分毫不差；
// 真实费用仍由成交回执落账。
func RoundTripCostFen(amount model.Fen) model.Fen {
	return BuyFeeFen(amount) + SellFeeFen(amount)
}

// RoundTripCostFundFen 双边成本估算（场内基金档）：无卖出印花税。
func RoundTripCostFundFen(amount model.Fen) model.Fen {
	return BuyFeeFen(amount) + SellFeeFundFen(amount)
}

// RoundTripCostFenFor 按标的品种取双边成本：isFund=true 走场内基金档。
//
// 收拢成一行是刻意的：品种判断散到各调用方迟早有人漏掉一处（ETF 补口上线时
// 漏掉的正是成本这一处，因为 IsETF 在 LLM/行业上限/卖出三处都分流了，看起来"已经分完了"）。
func RoundTripCostFenFor(amount model.Fen, isFund bool) model.Fen {
	if isFund {
		return RoundTripCostFundFen(amount)
	}
	return RoundTripCostFen(amount)
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
