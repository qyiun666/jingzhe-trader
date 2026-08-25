package broker

import (
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// pendingFill 待入账的成交单 (价格已在下单日按撮合规则确定, 到成交日才入账户)
type pendingFill struct {
	orderID string
	req     OrderRequest
	price   float64 // 含滑点且取整后的成交价
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
