package broker

import (
	"fmt"
	"sync"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// paperAccount PaperBroker 内部账户管理
// 不依赖 backtest 包, 独立实现以避免循环导入
type paperAccount struct {
	cash       float64
	positions  map[string]*model.Position
	costModel  *market.CostModel
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
}

func (pa *paperAccount) sell(tsCode string, qty int, price float64, cost model.Cost) {
	pos := pa.getOrCreatePosition(tsCode)
	amount := price * float64(qty)
	pa.cash += amount - cost.Total()
	market.OnSell(pos, qty, price, cost)
	if pos.TotalQty <= 0 {
		delete(pa.positions, tsCode)
	}
}

func (pa *paperAccount) cleanEmpty() {
	for tsCode, pos := range pa.positions {
		if pos.TotalQty <= 0 {
			delete(pa.positions, tsCode)
		}
	}
}

// PaperBroker 纸面交易券商 (模拟券商)
// 回测/纸面交易共用, 通过 Broker 接口统一执行路径
type PaperBroker struct {
	name           string
	account        *paperAccount
	oms            *OMS
	mu             sync.RWMutex
	tradeCallbacks []func(model.Trade)
	// 撮合相关 (回测模式使用)
	costModel      *market.CostModel
	currentDate    string
	nextDate       string
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

// SetTradeDate 设置当前交易日和下一交易日 (回测引擎调用)
func (pb *PaperBroker) SetTradeDate(current, next string) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.currentDate = current
	pb.nextDate = next
}

// PlaceOrder 下单
// 在纸面交易模式中, 订单直接成交 (基于简单价格模型)
// 在回测模式中, 由回测引擎传入成交结果
func (pb *PaperBroker) PlaceOrder(req OrderRequest) (string, error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	orderID := pb.oms.CreateOrder(req)
	pb.oms.SubmitOrder(orderID)

	// 检查方向
	if req.Side != model.SideBuy && req.Side != model.SideSell {
		pb.oms.RejectOrder(orderID, "未知方向")
		return orderID, fmt.Errorf("未知方向")
	}

	if req.Side == model.SideBuy {
		// 买入: 检查资金
		if req.Price <= 0 {
			pb.oms.RejectOrder(orderID, "买入价无效")
			return orderID, fmt.Errorf("买入价无效")
		}
		qty := market.RoundLot(req.Qty)
		if qty <= 0 {
			pb.oms.RejectOrder(orderID, "买入数量不足100股")
			return orderID, fmt.Errorf("买入数量不足100股")
		}
		buyCost := pb.costModel.BuyCost(req.Price, qty)
		if buyCost > pb.account.cash {
			// 资金不足, 减少数量
			maxQty := int(pb.account.cash/req.Price/100) * 100
			if maxQty <= 0 {
				pb.oms.RejectOrder(orderID, "资金不足")
				return orderID, fmt.Errorf("资金不足")
			}
			qty = maxQty
		}
		cost := pb.costModel.Calculate(model.SideBuy, req.Price, qty)
		pb.account.buy(req.TsCode, qty, req.Price, cost)
		trade := model.Trade{
			TsCode:      req.TsCode,
			Side:        model.SideBuy,
			Price:       req.Price,
			Qty:         qty,
			Amount:      req.Price * float64(qty),
			Commission:  cost.Commission,
			StampTax:    cost.StampTax,
			TransferFee: cost.TransferFee,
			TotalCost:   cost.Total(),
			TradeDate:   pb.currentDate,
			TradeTime:   pb.currentDate + " 093000",
		}
		pb.oms.FillOrder(orderID, trade)
		pb.notifyTrade(trade)
		logger.L().Infof("[PaperBroker] 买入 %s %d股 @%.2f 费用:%.2f",
			req.TsCode, qty, req.Price, cost.Total())
	} else {
		// 卖出
		pos, ok := pb.account.positions[req.TsCode]
		if !ok || pos.TotalQty <= 0 {
			pb.oms.RejectOrder(orderID, "无持仓")
			return orderID, fmt.Errorf("无持仓")
		}
		qty := req.Qty
		if pos.TotalQty < qty {
			qty = pos.TotalQty
		}
		if !market.CanSell(pos, qty) {
			if pos.AvailableQty > 0 {
				qty = pos.AvailableQty
			} else {
				pb.oms.RejectOrder(orderID, "T+1限制: 当日买入不可卖")
				return orderID, fmt.Errorf("T+1限制")
			}
		}
		cost := pb.costModel.Calculate(model.SideSell, req.Price, qty)
		pb.account.sell(req.TsCode, qty, req.Price, cost)
		trade := model.Trade{
			TsCode:      req.TsCode,
			Side:        model.SideSell,
			Price:       req.Price,
			Qty:         qty,
			Amount:      req.Price * float64(qty),
			Commission:  cost.Commission,
			StampTax:    cost.StampTax,
			TransferFee: cost.TransferFee,
			TotalCost:   cost.Total(),
			TradeDate:   pb.currentDate,
			TradeTime:   pb.currentDate + " 093000",
		}
		pb.oms.FillOrder(orderID, trade)
		pb.notifyTrade(trade)
		logger.L().Infof("[PaperBroker] 卖出 %s %d股 @%.2f 费用:%.2f",
			req.TsCode, qty, req.Price, cost.Total())
	}

	pb.account.cleanEmpty()
	return orderID, nil
}

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
	return &AssetInfo{
		Cash:        pb.account.cash,
		TotalAsset:  pb.account.totalAsset(),
		MarketValue: pb.account.marketValue(),
		Positions:   pb.account.positions,
	}, nil
}

func (pb *PaperBroker) OnTrade(callback func(model.Trade)) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.tradeCallbacks = append(pb.tradeCallbacks, callback)
}

func (pb *PaperBroker) SettleT1() {
	pb.account.settleT1()
}

func (pb *PaperBroker) notifyTrade(trade model.Trade) {
	for _, cb := range pb.tradeCallbacks {
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
