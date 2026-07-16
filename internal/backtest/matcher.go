package backtest

import (
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// FillPriceType 成交价类型
type FillPriceType int

const (
	FillNextOpen FillPriceType = iota // 次日开盘价成交
	FillClose                          // 当日收盘价成交
)

// Matcher 撮合器
// 模拟订单成交, 包含涨跌停检查、T+1检查、手数取整、费用计算
type Matcher struct {
	costModel  *market.CostModel
	fillPrice  FillPriceType
	slippage   float64 // 滑点 (bp)
	dataProvider *DataProvider
	limitRepo  limitProvider // 涨跌停价查询接口
}

// limitProvider 涨跌停价提供者接口
type limitProvider interface {
	GetByCodeAndDate(tsCode, tradeDate string) (*model.StkLimit, error)
}

// NewMatcher 创建撮合器
func NewMatcher(costModel *market.CostModel, fillPrice FillPriceType, slippage float64, dp *DataProvider, lr limitProvider) *Matcher {
	return &Matcher{
		costModel:    costModel,
		fillPrice:    fillPrice,
		slippage:     slippage,
		dataProvider: dp,
		limitRepo:    lr,
	}
}

// FillResult 成交结果
type FillResult struct {
	Signal  model.Signal
	Filled  bool
	Qty     int
	Price   float64
	Cost    model.Cost
	Reason  string // 未成交原因
}

// Match 处理信号, 返回成交结果
// tradeDate: 信号产生日
// nextDate: 下一交易日 (用于次日开盘价成交)
func (m *Matcher) Match(signal model.Signal, account *Account, tradeDate, nextDate string) FillResult {
	result := FillResult{Signal: signal}

	// 确定成交日期和成交价
	var fillDate string
	var fillBar *model.Bar
	if m.fillPrice == FillNextOpen {
		fillDate = nextDate
		fillBar = m.dataProvider.GetNextBar(signal.TsCode, tradeDate)
	} else {
		fillDate = tradeDate
		fillBar = m.dataProvider.GetBar(signal.TsCode, tradeDate)
	}

	if fillBar == nil {
		result.Reason = "无成交日行情数据"
		return result
	}

	// 确定成交价
	var fillPrice float64
	if m.fillPrice == FillNextOpen {
		fillPrice = fillBar.Open
	} else {
		fillPrice = fillBar.Close
	}

	// 应用滑点
	if signal.Direction == model.DirBuy {
		fillPrice *= (1 + m.slippage) // 买入价上浮
	} else {
		fillPrice *= (1 - m.slippage) // 卖出价下浮
	}
	fillPrice = market.RoundPrice(fillPrice)

	// 涨跌停检查
	if m.limitRepo != nil {
		limit, err := m.limitRepo.GetByCodeAndDate(signal.TsCode, fillDate)
		if err == nil && limit != nil {
			if err := market.CheckLimit(model.Side(signal.Direction), fillPrice, limit.UpLimit, limit.DownLimit); err != nil {
				result.Reason = fmt.Sprintf("涨跌停限制: %v", err)
				return result
			}
		}
	}

	// 处理买入信号
	if signal.Direction == model.DirBuy {
		qty := market.RoundLot(signal.TargetQty)
		if qty <= 0 {
			result.Reason = "买入数量不足100股"
			return result
		}

		// 检查资金是否充足
		buyCost := m.costModel.BuyCost(fillPrice, qty)
		if buyCost > account.Cash {
			// 资金不足, 减少买入数量
			maxQty := int(account.Cash/fillPrice/100) * 100
			if maxQty <= 0 {
				result.Reason = "资金不足"
				return result
			}
			qty = maxQty
			buyCost = m.costModel.BuyCost(fillPrice, qty)
		}

		cost := m.costModel.Calculate(model.SideBuy, fillPrice, qty)
		account.Buy(signal.TsCode, qty, fillPrice, cost)

		result.Filled = true
		result.Qty = qty
		result.Price = fillPrice
		result.Cost = cost
		return result
	}

	// 处理卖出信号
	if signal.Direction == model.DirSell {
		pos, ok := account.Positions[signal.TsCode]
		if !ok || pos.TotalQty <= 0 {
			result.Reason = "无持仓"
			return result
		}

		qty := signal.TargetQty
		if pos.TotalQty < qty {
			qty = pos.TotalQty // 卖出全部持仓
		}
		// T+1检查: 检查可卖量
		if !market.CanSell(pos, qty) {
			// 尝试卖出可卖部分
			if pos.AvailableQty > 0 {
				qty = pos.AvailableQty
			} else {
				result.Reason = "T+1限制: 当日买入不可卖"
				return result
			}
		}

		cost := m.costModel.Calculate(model.SideSell, fillPrice, qty)
		account.Sell(signal.TsCode, qty, fillPrice, cost)

		result.Filled = true
		result.Qty = qty
		result.Price = fillPrice
		result.Cost = cost
		return result
	}

	result.Reason = "未知信号方向"
	return result
}

// LogFill 记录成交日志
func LogFill(result FillResult, tradeDate string) {
	if result.Filled {
		dir := "买入"
		if result.Signal.Direction == model.DirSell {
			dir = "卖出"
		}
		logger.L().Infof("[%s] %s %s %d股 @%.2f 费用:%.2f 原因:%s",
			tradeDate, dir, result.Signal.TsCode, result.Qty, result.Price, result.Cost.Total(), result.Signal.Reason)
	} else {
		logger.L().Debugf("[%s] 未成交 %s 原因:%s", tradeDate, result.Signal.TsCode, result.Reason)
	}
}

// formatTime 格式化时间
func formatTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}
