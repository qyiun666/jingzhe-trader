package engine

import (
	"context"
	"fmt"
	"sync"

	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// PipelineConfig 执行管道配置
// 回测/模拟/实盘共用, 区别仅在于注入的 Broker 实现
type PipelineConfig struct {
	Broker    broker.Broker
	Strategy  strategy.Strategy
	Risk      *risk.RiskManager
	Data      *backtest.DataProvider
	Calendar  *market.Calendar
	Universe  []string
	StartDate string
	EndDate   string
	RunID     string                  // 运行批次ID (bt_* 回测 / live_* 实盘)
	TradeRepo *store.TradeRepo        // 成交/快照持久化, nil 时不落库
	FillMode  string                  // "next_open"(默认) 或 "close"
	Stocks    map[string]*model.Stock // 股票信息(风控用), nil 时按universe构建默认值
}

// Pipeline 统一执行管道
// 核心流程: T+1结算 → 行情 → 市值 → 止损信号 → 策略信号 → 风控 → 下单 → 落库 → 快照
type Pipeline struct {
	cfg       PipelineConfig
	stocks    map[string]*model.Stock
	mu        sync.Mutex // 保护 trades (OnTrade 回调可能来自其他 goroutine)
	trades    []model.Trade
	snapshots []model.AccountSnapshot
	// strategyErrDays 策略执行失败的天数 (OnBar 报错不应静默吞掉)
	strategyErrDays int
}

// NewPipeline 创建执行管道
func NewPipeline(cfg PipelineConfig) *Pipeline {
	stocks := cfg.Stocks
	if stocks == nil {
		stocks = make(map[string]*model.Stock, len(cfg.Universe))
		for _, code := range cfg.Universe {
			stocks[code] = &model.Stock{TsCode: code, ListStatus: "L"}
		}
	}
	return &Pipeline{cfg: cfg, stocks: stocks}
}

// Run 执行交易循环
func (p *Pipeline) Run() error {
	dates := p.cfg.Calendar.TradeDatesBetween(p.cfg.StartDate, p.cfg.EndDate)
	if len(dates) == 0 {
		return fmt.Errorf("交易区间内无交易日: %s ~ %s", p.cfg.StartDate, p.cfg.EndDate)
	}

	logger.L().Infof("执行管道启动: %s ~ %s, 交易日数: %d, 策略: %s, broker: %s, runID: %s",
		p.cfg.StartDate, p.cfg.EndDate, len(dates), p.cfg.Strategy.Name(), p.cfg.Broker.Name(), p.cfg.RunID)

	// 注册成交回调: 补充 RunID 并持久化
	p.cfg.Broker.OnTrade(p.onTrade)

	for i, date := range dates {
		nextDate := ""
		if i+1 < len(dates) {
			nextDate = dates[i+1]
		}
		p.runDay(date, nextDate)

		if (i+1)%50 == 0 && len(p.snapshots) > 0 {
			snap := p.snapshots[len(p.snapshots)-1]
			logger.L().Infof("[%s] 进度: %d/%d, 总资产: %.2f, 现金: %.2f",
				date, i+1, len(dates), snap.TotalAsset, snap.Cash)
		}
	}

	logger.L().Infof("执行管道完成, 共 %d 个交易日", len(dates))
	return nil
}

// runDay 执行单个交易日
func (p *Pipeline) runDay(date, nextDate string) {
	// 1. T+1 结算 (昨日买入转为可卖 + 到期的次日成交单入账)
	p.cfg.Broker.SettleT1(date)

	// 2. 构建当日行情
	bars := make(map[string]*model.Bar)
	for _, tsCode := range p.cfg.Universe {
		if bar := p.cfg.Data.GetBar(tsCode, date); bar != nil {
			bars[tsCode] = bar
		}
	}

	// 3. 更新持仓市值
	p.cfg.Broker.UpdateMarketValue(bars)

	// 4. 查询资产与持仓
	asset, err := p.cfg.Broker.QueryAsset()
	if err != nil {
		logger.L().Errorf("[%s] 查询资产失败: %v", date, err)
		return
	}
	positions, err := p.cfg.Broker.QueryPositions()
	if err != nil {
		logger.L().Errorf("[%s] 查询持仓失败: %v", date, err)
		return
	}

	// 5. 收集信号 (止损优先) 并过风控
	passed := p.collectSignals(date, bars, positions, asset)

	// 6. 执行信号
	p.executeSignals(date, nextDate, passed, bars)

	// 7. 记录账户快照
	p.recordSnapshot(date)
}

// collectSignals 全局止损信号 + 策略信号, 合并后统一过风控
// 返回按 "卖出在前" 排序的通过信号 (先卖出释放资金)
func (p *Pipeline) collectSignals(date string, bars map[string]*model.Bar,
	positions map[string]*model.Position, asset *broker.AssetInfo) []model.Signal {

	// 全局止损/止盈信号 (对所有持仓生效, 与策略无关)
	stopSignals := p.cfg.Risk.CheckStopLoss(positions, bars)
	stopCodes := StopCodesOf(stopSignals)
	for _, s := range stopSignals {
		logger.L().Infof("[%s] 全局风控信号 %s: %s", date, s.TsCode, s.Reason)
	}

	// 策略信号
	barCtx := &strategy.BarContext{
		TradeDate:  date,
		Universe:   p.cfg.Universe,
		Bars:       bars,
		Positions:  positions,
		Cash:       asset.Cash,
		TotalAsset: asset.TotalAsset,
		History:    p.cfg.Data,
	}
	stratSignals, err := p.cfg.Strategy.OnBar(context.Background(), barCtx)
	if err != nil {
		p.strategyErrDays++
		logger.L().Errorf("[%s] 策略 %s 执行出错(第%d天): %v", date, p.cfg.Strategy.Name(), p.strategyErrDays, err)
		stratSignals = nil
	}

	// 合并 (剔除重复/建议信号) → 风控检查 → 卖单优先排序 (与实盘共用同一套语义)
	merged := MergeStrategySignals(date, stopSignals, stopCodes, stratSignals)
	passed, _ := CheckAndSortSignals(date, p.cfg.Risk, merged, positions, asset.TotalAsset, p.stocks, bars)
	return passed
}

// executeSignals 执行通过风控的信号
func (p *Pipeline) executeSignals(date, nextDate string, signals []model.Signal, bars map[string]*model.Bar) {
	for _, sig := range signals {
		fillPrice, fillDate, fillBar := p.resolveFillPrice(sig.TsCode, date, nextDate)
		if fillPrice <= 0 {
			logger.L().Debugf("[%s] %s 无有效成交价, 跳过", date, sig.TsCode)
			continue
		}

		if pb, ok := p.cfg.Broker.(*broker.PaperBroker); ok {
			pb.SetTradeDate(date, nextDate)
		}

		side := model.SideBuy
		if sig.Direction == model.DirSell {
			side = model.SideSell
		}
		req := broker.OrderRequest{
			TsCode:   sig.TsCode,
			Side:     side,
			Qty:      sig.TargetQty,
			Price:    fillPrice,
			Reason:   sig.Reason,
			Strategy: p.cfg.Strategy.Name(),
			FillDate: fillDate,
		}
		if fillBar != nil {
			req.PreClose = fillBar.PreClose
		}
		if st := p.stocks[sig.TsCode]; st != nil {
			req.IsST = st.IsST
		}
		if _, err := p.cfg.Broker.PlaceOrder(req); err != nil {
			logger.L().Debugf("[%s] 下单未成交 %s: %v", date, sig.TsCode, err)
		}
	}
}

// resolveFillPrice 按成交模式确定成交价、成交日与成交K线
// next_open: 次日开盘价, 成交日取该股票的下一根K线日期 (停牌时自动顺延到复牌日);
// 无次日数据(回测末尾)回退当日收盘; close: 当日收盘价
func (p *Pipeline) resolveFillPrice(tsCode, date, nextDate string) (float64, string, *model.Bar) {
	if p.cfg.FillMode != "close" && nextDate != "" {
		if bar := p.cfg.Data.GetNextBar(tsCode, date); bar != nil && bar.Open > 0 {
			return bar.Open, bar.TradeDate, bar
		}
	}
	if bar := p.cfg.Data.GetBar(tsCode, date); bar != nil && bar.Close > 0 {
		return bar.Close, date, bar
	}
	return 0, "", nil
}

// onTrade 成交回调: 补充RunID, 持久化, 记入内存
func (p *Pipeline) onTrade(trade model.Trade) {
	trade.RunID = p.cfg.RunID
	p.mu.Lock()
	p.trades = append(p.trades, trade)
	p.mu.Unlock()

	if p.cfg.TradeRepo != nil {
		if _, err := p.cfg.TradeRepo.InsertTrade(&trade); err != nil {
			logger.L().Errorf("插入成交记录失败(runID=%s, %s %s): %v",
				p.cfg.RunID, trade.TradeDate, trade.TsCode, err)
		}
	}
}

// recordSnapshot 记录账户快照并持久化
func (p *Pipeline) recordSnapshot(date string) {
	asset, err := p.cfg.Broker.QueryAsset()
	if err != nil {
		logger.L().Errorf("[%s] 快照查询资产失败: %v", date, err)
		return
	}
	snap := model.AccountSnapshot{
		TradeDate:   date,
		TotalAsset:  asset.TotalAsset,
		Cash:        asset.Cash,
		MarketValue: asset.MarketValue,
	}
	if len(p.snapshots) > 0 {
		prev := p.snapshots[len(p.snapshots)-1]
		snap.PnL = snap.TotalAsset - prev.TotalAsset
		if prev.TotalAsset > 0 {
			snap.PnLPct = snap.PnL / prev.TotalAsset
		}
		initial := p.snapshots[0].TotalAsset
		if initial > 0 {
			snap.TotalPnL = snap.TotalAsset - initial
			snap.TotalPnLPct = snap.TotalPnL / initial
		}
	}
	p.snapshots = append(p.snapshots, snap)

	if p.cfg.TradeRepo != nil {
		if err := p.cfg.TradeRepo.InsertAccountSnapshot(p.cfg.RunID, snap); err != nil {
			logger.L().Errorf("插入账户快照失败(runID=%s, date=%s): %v", p.cfg.RunID, date, err)
		}
	}
}

// Snapshots 获取账户快照序列
func (p *Pipeline) Snapshots() []model.AccountSnapshot {
	return p.snapshots
}

// Trades 获取全部成交记录
func (p *Pipeline) Trades() []model.Trade {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]model.Trade, len(p.trades))
	copy(result, p.trades)
	return result
}

// StrategyErrorDays 返回策略执行失败的天数 (0=策略全程正常)
func (p *Pipeline) StrategyErrorDays() int {
	return p.strategyErrDays
}
