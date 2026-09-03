package risk

import (
	"fmt"

	"jingzhe-trader/internal/model"
)

// TargetQty 金额 → 股数：向下取整到 100 股（整手）。价格 ≤ 0 返回 0。
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

// PlanBuy 单笔买入的金额与股数核算（纯函数）：
//  1. 意向金额 = min(现金, 单票上限额度, 单笔上限额度)
//  2. 按价格折算整手股数
//  3. 校验最小交易额（低于下限返回错误，由调用方决定否决留痕）
//
// 返回实际可执行的金额（qty × price，可能小于意向金额）与股数。
func PlanBuy(p RiskParams, cash, price model.Fen) (model.Qty, model.Fen, error) {
	if price <= 0 {
		return 0, 0, fmt.Errorf("买入核算失败: 价格非法 (%d 分)", price)
	}
	if !p.AllowNewPosition {
		return 0, 0, fmt.Errorf("买入核算失败: 当前档位禁止开新仓")
	}
	intent := pctOf(p.TotalAsset, p.MaxPositionPct)
	if cap2 := pctOf(p.TotalAsset, p.MaxSingleAmountPct); cap2 < intent {
		intent = cap2
	}
	if cash < intent {
		intent = cash
	}
	qty := TargetQty(intent, price)
	amount := price.Mul(qty)
	if qty <= 0 || amount < p.MinSingleAmountFen {
		return 0, 0, fmt.Errorf("买入核算失败: 意向金额 %d 分低于单笔下限 %d 分（价格 %d 分）",
			int64(intent), int64(p.MinSingleAmountFen), int64(price))
	}
	return qty, amount, nil
}
