package trading

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// Loop 交易循环
// 回测和纸面交易共用此循环, 区别仅在于数据来源
// 核心流程: 每日开盘 → T+1结算 → 更新市值 → 策略信号 → 风控 → Broker下单 → 记录快照
type Loop struct {
	broker       broker.Broker
	strategy     strategy.Strategy
	riskManager  *risk.RiskManager
	dataProvider *backtest.DataProvider
	calendar     *market.Calendar
	universe     []string
	startDate    string
	endDate      string
	// 快照记录
	snapshots []model.AccountSnapshot
}

// NewLoop 创建交易循环
func NewLoop(
	br broker.Broker,
	strat strategy.Strategy,
	rm *risk.RiskManager,
	dp *backtest.DataProvider,
	cal *market.Calendar,
	universe []string,
	startDate, endDate string,
) *Loop {
	return &Loop{
		broker:       br,
		strategy:     strat,
		riskManager:  rm,
		dataProvider: dp,
		calendar:     cal,
		universe:     universe,
		startDate:    startDate,
		endDate:      endDate,
	}
}

// Run 执行交易循环
func (l *Loop) Run() error {
	tradeDates := l.calendar.TradeDatesBetween(l.startDate, l.endDate)
	if len(tradeDates) == 0 {
		return fmt.Errorf("交易区间内无交易日: %s ~ %s", l.startDate, l.endDate)
	}

	logger.L().Infof("开始交易循环: %s ~ %s, 交易日数: %d, 策略: %s",
		l.startDate, l.endDate, len(tradeDates), l.strategy.Name())

	// 收集股票信息用于风控
	// 使用已缓存的股票信息, 若无则默认允许交易
	stocks := make(map[string]*model.Stock)
	for _, code := range l.universe {
		stocks[code] = &model.Stock{TsCode: code, ListStatus: "L"}
	}

	for i, date := range tradeDates {
		// 1. T+1结算
		l.broker.SettleT1()

		// 2. 构建当日行情
		bars := make(map[string]*model.Bar)
		prices := make(map[string]float64)
		for _, tsCode := range l.universe {
			if bar := l.dataProvider.GetBar(tsCode, date); bar != nil {
				bars[tsCode] = bar
				prices[tsCode] = bar.Close
			}
		}

		// 3. 更新持仓市值
		l.broker.UpdateMarketValue(bars)

		// 4. 查询当前资产和持仓
		asset, _ := l.broker.QueryAsset()
		positions, _ := l.broker.QueryPositions()

		// 5. 构建策略上下文
		barCtx := &strategy.BarContext{
			TradeDate:  date,
			Universe:   l.universe,
			Bars:       bars,
			Positions:  positions,
			Cash:       asset.Cash,
			TotalAsset: asset.TotalAsset,
			History:    l.dataProvider,
		}

		// 6. 策略产生信号
		signals, err := l.strategy.OnBar(context.Background(), barCtx)
		if err != nil {
			logger.L().Errorf("[%s] 策略执行出错: %v", date, err)
			continue
		}

		// 7. 风控检查
		passedSignals, rejections := l.riskManager.Check(signals, positions, asset.TotalAsset, stocks, date, bars)
		for _, rej := range rejections {
			logger.L().Warnf("[%s] 风控拦截 %s: %s", date, rej.TsCode, rej.Reason)
		}

		// 8. 通过 Broker 执行信号
		// 确定成交价: 次日开盘价 (回测模式)
		nextDate := ""
		if i+1 < len(tradeDates) {
			nextDate = tradeDates[i+1]
		}

		for _, sig := range passedSignals {
			// 计算成交价
			var fillPrice float64
			if nextDate != "" {
				if bar := l.dataProvider.GetNextBar(sig.TsCode, date); bar != nil {
					fillPrice = bar.Open
				}
			}
			if fillPrice <= 0 {
				if bar := l.dataProvider.GetBar(sig.TsCode, date); bar != nil {
					fillPrice = bar.Close
				}
			}
			if fillPrice <= 0 {
				logger.L().Debugf("[%s] %s 无有效价格, 跳过", date, sig.TsCode)
				continue
			}

			// 设置 Broker 的当前交易日 (用于 PaperBroker 记录)
			if pb, ok := l.broker.(*broker.PaperBroker); ok {
				pb.SetTradeDate(date, nextDate)
			}

			var side model.Side
			if sig.Direction == model.DirBuy {
				side = model.SideBuy
			} else {
				side = model.SideSell
			}

			req := broker.OrderRequest{
				TsCode:   sig.TsCode,
				Side:     side,
				Qty:      sig.TargetQty,
				Price:    fillPrice,
				Reason:   sig.Reason,
				Strategy: l.strategy.Name(),
			}

			if _, err := l.broker.PlaceOrder(req); err != nil {
				logger.L().Debugf("[%s] 下单失败 %s: %v", date, sig.TsCode, err)
			}
		}

		// 9. 记录账户快照
		asset, _ = l.broker.QueryAsset()
		snap := model.AccountSnapshot{
			TradeDate:   date,
			TotalAsset:  asset.TotalAsset,
			Cash:        asset.Cash,
			MarketValue: asset.MarketValue,
		}
		if len(l.snapshots) > 0 {
			prev := l.snapshots[len(l.snapshots)-1]
			snap.PnL = snap.TotalAsset - prev.TotalAsset
			if prev.TotalAsset > 0 {
				snap.PnLPct = snap.PnL / prev.TotalAsset
			}
		}
		// 计算累计盈亏
		if len(l.snapshots) > 0 {
			initial := l.snapshots[0].TotalAsset
			if initial > 0 {
				snap.TotalPnL = snap.TotalAsset - initial
				snap.TotalPnLPct = snap.TotalPnL / initial
			}
		} else {
			snap.TotalPnL = 0
			snap.TotalPnLPct = 0
		}
		l.snapshots = append(l.snapshots, snap)

		if (i+1)%50 == 0 {
			logger.L().Infof("[%s] 进度: %d/%d, 总资产: %.2f, 现金: %.2f, 持仓: %d",
				date, i+1, len(tradeDates), snap.TotalAsset, snap.Cash, len(asset.Positions))
		}
	}

	logger.L().Infof("交易循环完成, 共 %d 个交易日", len(tradeDates))
	return nil
}

// Snapshots 获取账户快照
func (l *Loop) Snapshots() []model.AccountSnapshot {
	return l.snapshots
}

// Trades 获取所有成交记录
func (l *Loop) Trades() []model.Trade {
	if pb, ok := l.broker.(*broker.PaperBroker); ok {
		return pb.GetTrades()
	}
	return nil
}
