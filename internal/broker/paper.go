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

// SetMatchRules 配置撮合规则 (滑点 + 涨跌停检查)
func (pb *PaperBroker) SetMatchRules(slippage float64, limitRepo LimitProvider) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.slippage = slippage
	pb.limitRepo = limitRepo
}

func (pb *PaperBroker) Name() string { return pb.name }

// SetTradeDate 设置当前交易日和下一交易日 (回测引擎调用)
func (pb *PaperBroker) SetTradeDate(current, next string) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.currentDate = current
	pb.nextDate = next
}

// PlaceOrder 下单, 按撮合规则立即成交或拒绝
func (pb *PaperBroker) PlaceOrder(req OrderRequest) (string, error) {
	orderID, trade, err := pb.matchOrder(req)
	if err != nil {
		return orderID, err
	}
	// 锁外通知, 避免回调内再访问 broker 导致死锁
	pb.notifyTrade(trade)
	return orderID, nil
}

// matchOrder 锁内完成撮合全流程
func (pb *PaperBroker) matchOrder(req OrderRequest) (string, model.Trade, error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	orderID := pb.oms.CreateOrder(req)
	pb.oms.SubmitOrder(orderID)

	if req.Side != model.SideBuy && req.Side != model.SideSell {
		pb.oms.RejectOrder(orderID, "未知方向")
		return orderID, model.Trade{}, fmt.Errorf("未知方向")
	}
	if req.Price <= 0 {
		pb.oms.RejectOrder(orderID, "委托价无效")
		return orderID, model.Trade{}, fmt.Errorf("委托价无效")
	}

	// 应用滑点并取整价格
	price := req.Price
	if req.Side == model.SideBuy {
		price *= (1 + pb.slippage)
	} else {
		price *= (1 - pb.slippage)
	}
	price = market.RoundPrice(price)

	// 涨跌停检查: 涨停不买, 跌停不卖
	if err := pb.checkPriceLimit(req, price); err != nil {
		pb.oms.RejectOrder(orderID, err.Error())
		return orderID, model.Trade{}, err
	}

	var trade model.Trade
	var err error
	if req.Side == model.SideBuy {
		trade, err = pb.executeBuy(req, price)
	} else {
		trade, err = pb.executeSell(req, price)
	}
	if err != nil {
		pb.oms.RejectOrder(orderID, err.Error())
		return orderID, model.Trade{}, err
	}

	pb.oms.FillOrder(orderID, trade)
	pb.account.cleanEmpty()
	return orderID, trade, nil
}

// checkPriceLimit 涨跌停检查
func (pb *PaperBroker) checkPriceLimit(req OrderRequest, price float64) error {
	if pb.limitRepo == nil {
		return nil
	}
	fillDate := req.FillDate
	if fillDate == "" {
		fillDate = pb.currentDate
	}
	limit, err := pb.limitRepo.GetByCodeAndDate(req.TsCode, fillDate)
	if err != nil || limit == nil {
		return nil // 无涨跌停数据时不拦截
	}
	if err := market.CheckLimit(req.Side, price, limit.UpLimit, limit.DownLimit); err != nil {
		return fmt.Errorf("涨跌停限制: %w", err)
	}
	return nil
}

// executeBuy 执行买入: 含费资金检查, 资金不足时按含费总成本反推最大可买数量
func (pb *PaperBroker) executeBuy(req OrderRequest, price float64) (model.Trade, error) {
	qty := market.RoundLot(req.Qty)
	if qty <= 0 {
		return model.Trade{}, fmt.Errorf("买入数量不足100股")
	}

	cash := pb.account.cash
	if pb.costModel.BuyCost(price, qty) > cash {
		// 含手续费反推: 先预留安全边际估算, 再逐手递减直到总成本不超现金
		maxQty := market.RoundLot(int(cash * 0.998 / price))
		for maxQty > 0 && pb.costModel.BuyCost(price, maxQty) > cash {
			maxQty -= 100
		}
		if maxQty <= 0 {
			return model.Trade{}, fmt.Errorf("资金不足")
		}
		qty = maxQty
	}

	cost := pb.costModel.Calculate(model.SideBuy, price, qty)
	pb.account.buy(req.TsCode, qty, price, cost)
	trade := pb.buildTrade(req, model.SideBuy, price, qty, cost)
	logger.L().Infof("[PaperBroker] 买入 %s %d股 @%.2f 费用:%.2f", req.TsCode, qty, price, cost.Total())
	return trade, nil
}

// executeSell 执行卖出: 持仓与T+1检查
func (pb *PaperBroker) executeSell(req OrderRequest, price float64) (model.Trade, error) {
	pos, ok := pb.account.positions[req.TsCode]
	if !ok || pos.TotalQty <= 0 {
		return model.Trade{}, fmt.Errorf("无持仓")
	}
	qty := req.Qty
	if pos.TotalQty < qty {
		qty = pos.TotalQty
	}
	if !market.CanSell(pos, qty) {
		if pos.AvailableQty > 0 {
			qty = pos.AvailableQty
		} else {
			return model.Trade{}, fmt.Errorf("T+1限制: 当日买入不可卖")
		}
	}

	cost := pb.costModel.Calculate(model.SideSell, price, qty)
	pb.account.sell(req.TsCode, qty, price, cost)
	trade := pb.buildTrade(req, model.SideSell, price, qty, cost)
	logger.L().Infof("[PaperBroker] 卖出 %s %d股 @%.2f 费用:%.2f", req.TsCode, qty, price, cost.Total())
	return trade, nil
}

// buildTrade 构建成交记录
func (pb *PaperBroker) buildTrade(req OrderRequest, side model.Side, price float64, qty int, cost model.Cost) model.Trade {
	tradeDate := pb.currentDate
	if req.FillDate != "" {
		tradeDate = req.FillDate
	}
	return model.Trade{
		TsCode:      req.TsCode,
		Side:        side,
		Price:       price,
		Qty:         qty,
		Amount:      price * float64(qty),
		Commission:  cost.Commission,
		StampTax:    cost.StampTax,
		TransferFee: cost.TransferFee,
		TotalCost:   cost.Total(),
		TradeDate:   tradeDate,
		TradeTime:   tradeDate + " 093000",
	}
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

func (pb *PaperBroker) SettleT1() {
	pb.account.settleT1()
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
