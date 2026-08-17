package broker

import (
	"fmt"
	"sync"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
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

// pendingFill 待入账的成交单 (价格已在下单日按撮合规则确定, 到成交日才入账户)
type pendingFill struct {
	orderID string
	req     OrderRequest
	price   float64 // 含滑点且取整后的成交价
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

// SetAdjRatioFn 设置复权因子比值函数 (回测中 DataProvider 提供), 用于涨跌停检查的价格口径换算
func (pb *PaperBroker) SetAdjRatioFn(fn func(tsCode, tradeDate string) float64) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.adjRatioFn = fn
}

func (pb *PaperBroker) Name() string { return pb.name }

// SetTradeDate 设置当前交易日和下一交易日 (回测引擎调用)
func (pb *PaperBroker) SetTradeDate(current, next string) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	pb.currentDate = current
	pb.nextDate = next
}

// PlaceOrder 下单, 按撮合规则成交或拒绝
// FillDate 晚于当前交易日时登记为待成交单, 到成交日的 SettleT1 才真正入账 (模拟次日开盘价成交)
func (pb *PaperBroker) PlaceOrder(req OrderRequest) (string, error) {
	orderID, trade, pending, err := pb.matchOrder(req)
	if err != nil || pending {
		return orderID, err
	}
	// 锁外通知, 避免回调内再访问 broker 导致死锁
	pb.notifyTrade(trade)
	return orderID, nil
}

// matchOrder 锁内完成撮合全流程; pending=true 表示已登记为待成交单(未入账)
func (pb *PaperBroker) matchOrder(req OrderRequest) (string, model.Trade, bool, error) {
	pb.mu.Lock()
	defer pb.mu.Unlock()

	orderID := pb.oms.CreateOrder(req)
	pb.oms.SubmitOrder(orderID)

	if req.Side != model.SideBuy && req.Side != model.SideSell {
		pb.oms.RejectOrder(orderID, "未知方向")
		return orderID, model.Trade{}, false, fmt.Errorf("未知方向")
	}
	if req.Price <= 0 {
		pb.oms.RejectOrder(orderID, "委托价无效")
		return orderID, model.Trade{}, false, fmt.Errorf("委托价无效")
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
		return orderID, model.Trade{}, false, err
	}

	// 未来日成交: 登记待成交单, 到成交日 SettleT1 时才做资金/持仓检查并入账
	// currentDate 为空(未走回测循环的直接调用)时按立即成交处理, 避免单子永久挂起
	if pb.currentDate != "" && req.FillDate != "" && req.FillDate > pb.currentDate {
		pf := pendingFill{orderID: orderID, req: req, price: price}
		pf.req.Price = price
		pb.pending = append(pb.pending, pf)
		return orderID, model.Trade{}, true, nil
	}

	trade, err := pb.executeAtMarket(orderID, req, price)
	if err != nil {
		return orderID, model.Trade{}, false, err
	}
	return orderID, trade, false, nil
}

// executeAtMarket 立即执行买卖 (调用方须持锁); 失败时拒绝订单
func (pb *PaperBroker) executeAtMarket(orderID string, req OrderRequest, price float64) (model.Trade, error) {
	var trade model.Trade
	var err error
	if req.Side == model.SideBuy {
		trade, err = pb.executeBuy(req, price)
	} else {
		trade, err = pb.executeSell(req, price)
	}
	if err != nil {
		pb.oms.RejectOrder(orderID, err.Error())
		return model.Trade{}, err
	}
	pb.oms.FillOrder(orderID, trade)
	pb.account.cleanEmpty()
	return trade, nil
}

// checkPriceLimit 涨跌停检查
// 成交价可能是前复权价, 而涨跌停价是原始价: 先按复权因子比换算回原始价再比较
// 无涨跌停数据时, 用成交日昨收价按板块规则自算兜底 (不再直接放行)
func (pb *PaperBroker) checkPriceLimit(req OrderRequest, price float64) error {
	fillDate := req.FillDate
	if fillDate == "" {
		fillDate = pb.currentDate
	}
	ratio := 1.0
	if pb.adjRatioFn != nil {
		if r := pb.adjRatioFn(req.TsCode, fillDate); r > 0 {
			ratio = r
		}
	}
	rawPrice := market.RoundPrice(price / ratio)

	if pb.limitRepo != nil {
		if limit, err := pb.limitRepo.GetByCodeAndDate(req.TsCode, fillDate); err == nil && limit != nil {
			if err := market.CheckLimit(req.Side, rawPrice, limit.UpLimit, limit.DownLimit); err != nil {
				return fmt.Errorf("涨跌停限制: %w", err)
			}
			return nil
		}
	}

	// 无涨跌停数据: 自算兜底
	if req.PreClose <= 0 {
		return nil // 无昨收价无法自算, 放行
	}
	rawPreClose := req.PreClose / ratio
	date, err := time.Parse("20060102", fillDate)
	if err != nil {
		return nil
	}
	up := market.CalcUpLimit(rawPreClose, req.TsCode, req.IsST, date)
	down := market.CalcDownLimit(rawPreClose, req.TsCode, req.IsST, date)
	if err := market.CheckLimit(req.Side, rawPrice, up, down); err != nil {
		return fmt.Errorf("涨跌停限制(自算): %w", err)
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
		Strategy:    req.Strategy,
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
