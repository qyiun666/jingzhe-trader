package market

import "jingzhe-trader/internal/model"

// ===================== 涨跌停 / 停牌 / 整手 / T+1 规则 =====================
// 涨跌停判定用 stk_limit 价格字段，绝不依赖状态编码猜测（D1）。

// IsLimitUp 是否涨停（价格 ≥ 涨停价）。
func IsLimitUp(price, upLimit model.Fen) bool {
	return price >= upLimit
}

// IsLimitDown 是否跌停（价格 ≤ 跌停价）。
func IsLimitDown(price, downLimit model.Fen) bool {
	return price <= downLimit
}

// IsSuspended 是否停牌（suspend 标记）。
func IsSuspended(suspended bool) bool {
	return suspended
}

// CanBuy 涨停禁买：涨停时不可买入。
func CanBuy(price, upLimit model.Fen) bool {
	return !IsLimitUp(price, upLimit)
}

// CanSell 跌停禁卖：跌停时不可卖出。
func CanSell(price, downLimit model.Fen) bool {
	return !IsLimitDown(price, downLimit)
}

// RoundLotDown 向下取整到 100 股。
func RoundLotDown(q model.Qty) model.Qty {
	return q.RoundLotDown()
}

// RoundLotUp 向上取整到 100 股。
func RoundLotUp(q model.Qty) model.Qty {
	return q.RoundLotUp()
}

// T1Available T+1 可卖量 = 总持仓 − 当日买入。
func T1Available(total, todayBought model.Qty) model.Qty {
	avail := total - todayBought
	if avail < 0 {
		avail = 0
	}
	return avail
}

// MinTradeAmountOK 是否满足最小交易金额门槛（amount 为金额分）。
func MinTradeAmountOK(amount model.Fen, minAmount model.Fen) bool {
	return amount >= minAmount
}
