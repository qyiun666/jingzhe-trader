package risk

import (
	"jingzhe-trader/internal/model"
)

// TargetQty 金额 → 股数：向下取整到一手（model.LotShares 股）。价格 ≤ 0 返回 0。
func TargetQty(amount, price model.Fen) model.Qty {
	if price <= 0 || amount <= 0 {
		return 0
	}
	q := amount / price // 整数除法即向零取整
	return model.Qty(q).RoundLotDown()
}

// pctOf 金额 × 比例（分）。
func pctOf(total model.Fen, r float64) model.Fen {
	return model.FromFloat(total.Float() * r)
}
