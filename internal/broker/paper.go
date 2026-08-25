package broker

import (
	"sync"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// PaperBroker 纸面交易券商 (模拟券商)
// 回测/纸面交易共用唯一模拟账户, 通过 Broker 接口统一执行路径
// 撮合规则: 滑点 -> 价格取整 -> 涨跌停检查 -> 含费资金检查/T+1 -> 成交
type PaperBroker struct {
	name           string
	account        *paperAccount
	oms            *OMS
	mu             sync.RWMutex
	tradeCallbacks []func(model.Trade)
	// 撮合相关
	costModel   *market.CostModel
	slippage    float64       // 滑点比例 (买入上浮/卖出下浮)
	limitRepo   LimitProvider // 涨跌停价查询, 可为nil
	currentDate string
	nextDate    string
	// pendingFills 次日/未来成交的待入账成交单 (T日信号按T+1开盘价成交, T+1开盘结算时才入账)
	pending []pendingFill
	// adjRatioFn 返回前复权因子比值 (adj_fillDate/adj_latest), 用于将复权成交价换算回原始价与涨跌停价比较; nil 时不换算
	adjRatioFn func(tsCode, tradeDate string) float64
}

// NewPaperBroker 创建纸面交易券商
func NewPaperBroker(name string, initialCapital float64, costModel *market.CostModel) *PaperBroker {
	return &PaperBroker{
		name:      name,
		account:   newPaperAccount(initialCapital, costModel),
		costModel: costModel,
		oms:       NewOMS(),
	}
}

func (pb *PaperBroker) Name() string { return pb.name }

func (pb *PaperBroker) CancelOrder(orderID string) error {
	pb.oms.CancelOrder(orderID)
	return nil
}

func (pb *PaperBroker) QueryPositions() (map[string]*model.Position, error) {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	result := make(map[string]*model.Position, len(pb.account.positions))
	for k, v := range pb.account.positions {
		pos := *v
		result[k] = &pos
	}
	return result, nil
}

func (pb *PaperBroker) QueryAsset() (*AssetInfo, error) {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	// 深拷贝持仓, 避免外部持有内部map引用导致并发问题
	positionsCopy := make(map[string]*model.Position, len(pb.account.positions))
	for k, v := range pb.account.positions {
		pos := *v
		positionsCopy[k] = &pos
	}
	return &AssetInfo{
		Cash:        pb.account.cash,
		TotalAsset:  pb.account.totalAsset(),
		MarketValue: pb.account.marketValue(),
		Positions:   positionsCopy,
	}, nil
}

func (pb *PaperBroker) OnTrade(callback func(model.Trade)) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.tradeCallbacks = append(pb.tradeCallbacks, callback)
}

// SettleT1 每日开盘前结算:
//  1. 昨日买入转入可卖 (T+1 交收)
//  2. 成交日 <= date 的待成交单按下单时确定的成交价入账 (次开盘价成交模型)
//
// 入账时做资金/可卖检查, 失败拒绝并记日志 (如卖出回款未到时买入资金不足)
func (pb *PaperBroker) SettleT1(date string) {
	pb.mu.Lock()
	pb.account.settleT1()

	var filled []model.Trade
	if len(pb.pending) > 0 {
		remaining := pb.pending[:0]
		for _, pf := range pb.pending {
			if pf.req.FillDate > date {
				remaining = append(remaining, pf)
				continue
			}
			trade, err := pb.executeAtMarket(pf.orderID, pf.req, pf.price)
			if err != nil {
				logger.L().Warnf("[PaperBroker] 待成交单入账失败 %s %s 成交日%s: %v",
					pf.req.TsCode, pf.req.Side, pf.req.FillDate, err)
				continue
			}
			filled = append(filled, trade)
		}
		pb.pending = remaining
	}
	pb.mu.Unlock()

	// 锁外通知成交
	for _, trade := range filled {
		pb.notifyTrade(trade)
	}
}

func (pb *PaperBroker) notifyTrade(trade model.Trade) {
	// 锁内拷贝快照后锁外遍历, 避免与 OnTrade 注册并发产生 data race
	pb.mu.RLock()
	callbacks := make([]func(model.Trade), len(pb.tradeCallbacks))
	copy(callbacks, pb.tradeCallbacks)
	pb.mu.RUnlock()

	for _, cb := range callbacks {
		cb(trade)
	}
}

// GetOMS 获取OMS实例
func (pb *PaperBroker) GetOMS() *OMS {
	return pb.oms
}

// GetCash 获取现金
func (pb *PaperBroker) GetCash() float64 {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	return pb.account.cash
}

// GetPositions 获取持仓 (直接引用, 注意并发安全)
func (pb *PaperBroker) GetPositions() map[string]*model.Position {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	return pb.account.positions
}

// UpdateMarketValue 更新持仓市值 (回测引擎调用)
func (pb *PaperBroker) UpdateMarketValue(bars map[string]*model.Bar) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.account.updateMarketValue(bars)
}

// TotalAsset 总资产
func (pb *PaperBroker) TotalAsset() float64 {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	return pb.account.totalAsset()
}

// GetTrades 获取所有成交记录
func (pb *PaperBroker) GetTrades() []model.Trade {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	var trades []model.Trade
	for _, rec := range pb.oms.GetAllOrders() {
		trades = append(trades, rec.Trades...)
	}
	return trades
}

// ImportPositions 从外部导入持仓（覆盖当前持仓）
// 用于用户同步真实持仓到系统
func (pb *PaperBroker) ImportPositions(positions map[string]*model.Position, cash float64) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.account.positions = positions
	pb.account.cash = cash
}

// RecordTrade 记录一笔已执行的交易（不通过 PlaceOrder）
// 用于用户执行交易后反馈给系统
func (pb *PaperBroker) RecordTrade(tsCode string, side model.Side, qty int, price float64) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	cost := pb.costModel.Calculate(side, price, qty)
	if side == model.SideBuy {
		pb.account.buy(tsCode, qty, price, cost)
	} else {
		pb.account.sell(tsCode, qty, price, cost)
	}
}
