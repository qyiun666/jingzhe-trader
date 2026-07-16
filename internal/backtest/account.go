package backtest

import (
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/market"
)

// Account 回测账户
type Account struct {
	Cash       float64                       // 可用现金
	InitialCapital float64                    // 初始资金
	Positions  map[string]*model.Position    // 持仓
	LastTotalAsset float64                    // 上一交易日总资产 (用于计算当日盈亏)
}

// NewAccount 创建回测账户
func NewAccount(initialCapital float64) *Account {
	return &Account{
		Cash:            initialCapital,
		InitialCapital:  initialCapital,
		LastTotalAsset:  initialCapital,
		Positions:       make(map[string]*model.Position),
	}
}

// SettleT1 结算T+1 (每个交易日开盘前调用)
func (a *Account) SettleT1() {
	market.SettleT1(a.Positions)
}

// GetOrCreatePosition 获取或创建持仓
func (a *Account) GetOrCreatePosition(tsCode string) *model.Position {
	if pos, ok := a.Positions[tsCode]; ok {
		return pos
	}
	pos := &model.Position{TsCode: tsCode}
	a.Positions[tsCode] = pos
	return pos
}

// UpdateMarketValue 按最新价更新持仓市值
func (a *Account) UpdateMarketValue(bars map[string]*model.Bar) {
	for tsCode, pos := range a.Positions {
		if pos.TotalQty <= 0 {
			continue
		}
		bar, ok := bars[tsCode]
		if !ok {
			continue
		}
		pos.MarketPrice = bar.Close
		pos.MarketValue = bar.Close * float64(pos.TotalQty)
		if pos.CostPrice > 0 {
			pos.FloatingPnL = pos.MarketValue - pos.CostPrice*float64(pos.TotalQty)
			pos.FloatingPnLPct = pos.FloatingPnL / (pos.CostPrice * float64(pos.TotalQty))
		}
	}
}

// TotalAsset 总资产 = 现金 + 持仓市值
func (a *Account) TotalAsset() float64 {
	total := a.Cash
	for _, pos := range a.Positions {
		if pos.TotalQty > 0 {
			total += pos.MarketValue
		}
	}
	return total
}

// MarketValue 持仓总市值
func (a *Account) MarketValue() float64 {
	mv := 0.0
	for _, pos := range a.Positions {
		if pos.TotalQty > 0 {
			mv += pos.MarketValue
		}
	}
	return mv
}

// Snapshot 生成账户快照
func (a *Account) Snapshot(tradeDate string) model.AccountSnapshot {
	totalAsset := a.TotalAsset()
	pnl := totalAsset - a.LastTotalAsset
	pnlPct := 0.0
	if a.LastTotalAsset > 0 {
		pnlPct = pnl / a.LastTotalAsset
	}
	totalPnL := totalAsset - a.InitialCapital
	totalPnLPct := 0.0
	if a.InitialCapital > 0 {
		totalPnLPct = totalPnL / a.InitialCapital
	}

	return model.AccountSnapshot{
		TradeDate:   tradeDate,
		TotalAsset:  totalAsset,
		Cash:        a.Cash,
		MarketValue: a.MarketValue(),
		PnL:         pnl,
		PnLPct:      pnlPct,
		TotalPnL:    totalPnL,
		TotalPnLPct: totalPnLPct,
	}
}

// Buy 买入成交
func (a *Account) Buy(tsCode string, qty int, price float64, cost model.Cost) {
	pos := a.GetOrCreatePosition(tsCode)
	// 扣减现金 (买入花费 = 成交金额 + 费用)
	amount := price * float64(qty)
	a.Cash -= amount + cost.Total()
	market.OnBuy(pos, qty, price, cost)
}

// Sell 卖出成交
func (a *Account) Sell(tsCode string, qty int, price float64, cost model.Cost) {
	pos := a.GetOrCreatePosition(tsCode)
	// 增加现金 (卖出收入 = 成交金额 - 费用)
	amount := price * float64(qty)
	a.Cash += amount - cost.Total()
	market.OnSell(pos, qty, price, cost)
	// 清理零持仓
	if pos.TotalQty <= 0 {
		delete(a.Positions, tsCode)
	}
}

// CleanEmptyPositions 清理空持仓
func (a *Account) CleanEmptyPositions() {
	for tsCode, pos := range a.Positions {
		if pos.TotalQty <= 0 {
			delete(a.Positions, tsCode)
		}
	}
}
