package market

import "jingzhe-trader/internal/model"

// T+1 结算规则
//
// A 股实行 T+1 交收制度: 当日买入的股票, 次一交易日才能卖出。
//
// 持仓模型 (model.Position) 中:
//   - AvailableQty: 可卖量 (T+1: 今日买入量不计入可卖)
//   - TodayBought:  今日买入量 (次一交易日开盘前转入 AvailableQty)
//
// 每个交易日开盘前需调用 SettleT1, 将各持仓的 TodayBought 转入 AvailableQty。

// SettleT1 结算 T+1: 将今日买入量转入可卖量
// 应在每个交易日开盘前调用一次, 完成昨日买入的 T+1 交收
func SettleT1(positions map[string]*model.Position) {
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		// 昨日买入的股票今日可卖
		pos.AvailableQty += pos.TodayBought
		pos.TodayBought = 0
	}
}

// CanSell 检查是否可卖出指定数量
// 可卖量 (AvailableQty) 必须大于等于卖出数量
// 注: 当日买入量 (TodayBought) 受 T+1 限制, 不计入可卖量
func CanSell(pos *model.Position, qty int) bool {
	if pos == nil || qty <= 0 {
		return false
	}
	return pos.AvailableQty >= qty
}

// OnBuy 买入成交后更新持仓 (T+1: 当日买入计入 TodayBought, 不计入可卖量)
// 成本价采用移动加权平均法, 买入费用 (佣金 + 过户费) 计入持仓成本
func OnBuy(pos *model.Position, qty int, price float64, cost model.Cost) {
	if pos == nil || qty <= 0 || price <= 0 {
		return
	}

	// 先记录原持仓量与原成本金额
	oldQty := pos.TotalQty
	oldCostAmount := pos.CostPrice * float64(oldQty)

	// 本次买入计入成本金额 (含买入费用: 佣金 + 过户费, 买入无印花税)
	buyCostAmount := price*float64(qty) + cost.Commission + cost.TransferFee

	// 更新持仓量 (当日买入量单独累计, T+1 后才可卖)
	pos.TotalQty = oldQty + qty
	pos.TodayBought += qty

	// 移动加权平均成本
	if pos.TotalQty > 0 {
		pos.CostPrice = (oldCostAmount + buyCostAmount) / float64(pos.TotalQty)
	}
}

// OnSell 卖出成交后更新持仓
// 卖出按移动加权法处理, 剩余持仓的成本价保持不变
// 调用前应先通过 CanSell 检查可卖量是否充足
func OnSell(pos *model.Position, qty int, price float64, cost model.Cost) {
	if pos == nil || qty <= 0 {
		return
	}

	// 可卖量不足时, 仅扣减可卖部分 (防御性处理, 正常流程应先调用 CanSell)
	if pos.AvailableQty < qty {
		qty = pos.AvailableQty
	}
	if qty <= 0 {
		return
	}

	// 扣减持仓量与可卖量
	pos.TotalQty -= qty
	pos.AvailableQty -= qty

	// 防止浮点/异常导致负数
	if pos.TotalQty < 0 {
		pos.TotalQty = 0
	}
	if pos.AvailableQty < 0 {
		pos.AvailableQty = 0
	}

	// 卖出不改变剩余持仓的成本价 (移动加权法)
}
