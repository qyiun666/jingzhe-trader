package broker

import (
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// paperAccount PaperBroker 内部账户管理
// 不依赖 backtest 包, 独立实现以避免循环导入
type paperAccount struct {
	cash      float64
	positions map[string]*model.Position
	costModel *market.CostModel
}

func newPaperAccount(initialCapital float64, costModel *market.CostModel) *paperAccount {
	return &paperAccount{
		cash:      initialCapital,
		positions: make(map[string]*model.Position),
		costModel: costModel,
	}
}

func (pa *paperAccount) settleT1() {
	market.SettleT1(pa.positions)
}

func (pa *paperAccount) getOrCreatePosition(tsCode string) *model.Position {
	if pos, ok := pa.positions[tsCode]; ok {
		return pos
	}
	pos := &model.Position{TsCode: tsCode}
	pa.positions[tsCode] = pos
	return pos
}

func (pa *paperAccount) updateMarketValue(bars map[string]*model.Bar) {
	for tsCode, pos := range pa.positions {
		if pos.TotalQty <= 0 {
			continue
		}
		bar, ok := bars[tsCode]
		if !ok {
			continue
		}
		pos.MarketPrice = bar.Close
		pos.MarketValue = bar.Close * float64(pos.TotalQty)
		if bar.Close > pos.HighPrice {
			pos.HighPrice = bar.Close // 维护持仓期间最高价 (移动止盈基准)
		}
		if pos.CostPrice > 0 {
			pos.FloatingPnL = pos.MarketValue - pos.CostPrice*float64(pos.TotalQty)
			pos.FloatingPnLPct = pos.FloatingPnL / (pos.CostPrice * float64(pos.TotalQty))
		}
	}
}

func (pa *paperAccount) totalAsset() float64 {
	total := pa.cash
	for _, pos := range pa.positions {
		if pos.TotalQty > 0 {
			total += pos.MarketValue
		}
	}
	return total
}

func (pa *paperAccount) marketValue() float64 {
	mv := 0.0
	for _, pos := range pa.positions {
		if pos.TotalQty > 0 {
			mv += pos.MarketValue
		}
	}
	return mv
}

func (pa *paperAccount) buy(tsCode string, qty int, price float64, cost model.Cost) {
	pos := pa.getOrCreatePosition(tsCode)
	amount := price * float64(qty)
	pa.cash -= amount + cost.Total()
	market.OnBuy(pos, qty, price, cost)
	// 成交价即时更新市值, 避免新仓当日快照市值为0造成假回撤
	pos.MarketPrice = price
	pos.MarketValue = price * float64(pos.TotalQty)
}

func (pa *paperAccount) sell(tsCode string, qty int, price float64, cost model.Cost) {
	pos := pa.getOrCreatePosition(tsCode)
	amount := price * float64(qty)
	pa.cash += amount - cost.Total()
	market.OnSell(pos, qty, price, cost)
	if pos.TotalQty <= 0 {
		delete(pa.positions, tsCode)
		return
	}
	// 部分卖出: 按成交价刷新剩余持仓市值
	pos.MarketPrice = price
	pos.MarketValue = price * float64(pos.TotalQty)
}

func (pa *paperAccount) cleanEmpty() {
	for tsCode, pos := range pa.positions {
		if pos.TotalQty <= 0 {
			delete(pa.positions, tsCode)
		}
	}
}
