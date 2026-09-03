package market

import (
	"math"

	"jingzhe-trader/internal/model"
)

// TradeCost 单笔交易费用明细（全部为分）。
type TradeCost struct {
	Commission  model.Fen // 佣金
	StampTax    model.Fen // 印花税（仅卖出）
	TransferFee model.Fen // 过户费
	TotalFee    model.Fen // 费用合计
	TotalCost   model.Fen // 买入=金额+费用；卖出=金额−费用（到账）
}

// CostParams 交易成本参数集（来自 config cost.* 键，由调用方装配）。
type CostParams struct {
	CommissionRate  float64   // 佣金费率（如 0.00025）
	MinCommission   model.Fen // 最低佣金（分）
	StampTaxRate    float64   // 印花税率（仅卖出，如 0.001）
	TransferFeeRate float64   // 过户费率（如 0.00001）
}

// CalcCommission 佣金 = max(金额 × 费率, 最低佣金)。金额单位为分，费率为比例（如 0.00025）。
func CalcCommission(amount model.Fen, rate float64, minCommission model.Fen) model.Fen {
	c := model.Fen(math.Round(float64(amount) * rate))
	if c < minCommission {
		c = minCommission
	}
	return c
}

// CalcStampTax 印花税 = 金额 × 印花税率（仅卖出，如 0.001）。
func CalcStampTax(amount model.Fen, rate float64) model.Fen {
	return model.Fen(math.Round(float64(amount) * rate))
}

// CalcTransferFee 过户费 = 金额 × 过户费率（如 0.00001）。
func CalcTransferFee(amount model.Fen, rate float64) model.Fen {
	return model.Fen(math.Round(float64(amount) * rate))
}

// CalcTradeCost 计算单笔交易费用（买入含佣金+过户费；卖出另加印花税）。
// 费率（commissionRate/stampTaxRate/transferFeeRate）为比例浮点；最低佣金 minCommission 为金额（分，model.Fen）。
func CalcTradeCost(amount model.Fen, isBuy bool, commissionRate, stampTaxRate, transferFeeRate float64, minCommission model.Fen) TradeCost {
	var tc TradeCost
	tc.Commission = CalcCommission(amount, commissionRate, minCommission)
	tc.TransferFee = CalcTransferFee(amount, transferFeeRate)
	if !isBuy {
		tc.StampTax = CalcStampTax(amount, stampTaxRate)
	}
	tc.TotalFee = tc.Commission + tc.StampTax + tc.TransferFee
	if isBuy {
		tc.TotalCost = amount + tc.TotalFee
	} else {
		tc.TotalCost = amount - tc.TotalFee
	}
	return tc
}
