package backtest

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// EngineConfig 回测引擎配置
type EngineConfig struct {
	StartDate      string
	EndDate        string
	InitialCapital float64
	Universe       []string // 股票池
	Benchmark      string   // 基准代码
	Slippage       float64
	FillPrice      string // "next_open" 或 "close"
	StrategyName   string
	StrategyParams map[string]interface{}
	Silent         bool // 静默模式: 不打印回测摘要 (用于参数优化等批量场景)
}

// Engine 回测引擎
type Engine struct {
	cfg          EngineConfig
	db           *sqlx.DB
	barRepo      *store.BarRepo
	calRepo      *store.CalendarRepo
	limitRepo    *store.LimitRepo
	tradeRepo    *store.TradeRepo
	dataProvider *DataProvider
	matcher      *Matcher
	account      *Account
	strategy     strategy.Strategy
	calendar     *market.Calendar
	runID        string
}

// NewEngine 创建回测引擎
func NewEngine(cfg EngineConfig, appCfg *config.Config) (*Engine, error) {
	db, err := store.NewDB(appCfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 获取交易日历
	calRepo := store.NewCalendarRepo(db)
	cals, err := calRepo.GetTradeDays(cfg.StartDate, cfg.EndDate)
	if err != nil {
		return nil, fmt.Errorf("获取交易日历失败: %w", err)
	}
	if len(cals) == 0 {
		return nil, fmt.Errorf("回测区间内无交易日: %s ~ %s", cfg.StartDate, cfg.EndDate)
	}

	tradeDates := make([]string, len(cals))
	for i, c := range cals {
		tradeDates[i] = c.CalDate
	}
	calendar := market.NewCalendar(tradeDates)

	// 预加载数据 (提前多加载一些历史数据用于指标计算)
	// 向前推1年以确保有足够的历史数据
	preStartDate := shiftDate(cfg.StartDate, -365)
	dataProvider, err := NewDataProvider(store.NewBarRepo(db), cfg.Universe, preStartDate, cfg.EndDate)
	if err != nil {
		return nil, fmt.Errorf("加载数据失败: %w", err)
	}

	// 过滤无数据的股票
	validUniverse := make([]string, 0, len(cfg.Universe))
	for _, code := range cfg.Universe {
		if bars, ok := dataProvider.barsByCode[code]; ok && len(bars) > 0 {
			validUniverse = append(validUniverse, code)
		}
	}
	if len(validUniverse) == 0 {
		return nil, fmt.Errorf("股票池中无有效数据")
	}
	logger.L().Infof("有效股票池: %d/%d", len(validUniverse), len(cfg.Universe))

	// 费用模型
	costModel := market.NewCostModel(appCfg.Cost)

	// 成交价类型
	var fillType FillPriceType
	if cfg.FillPrice == "close" {
		fillType = FillClose
	} else {
		fillType = FillNextOpen
	}

	// 撮合器
	limitRepo := store.NewLimitRepo(db)
	matcher := NewMatcher(costModel, fillType, cfg.Slippage, dataProvider, limitRepo)

	// 策略
	registry := strategy.DefaultRegistry()
	strat, ok := registry.Get(cfg.StrategyName)
	if !ok {
		return nil, fmt.Errorf("未知策略: %s, 可用策略: %v", cfg.StrategyName, registry.Names())
	}
	if err := strat.Init(context.Background(), cfg.StrategyParams); err != nil {
		return nil, fmt.Errorf("策略初始化失败: %w", err)
	}

	// 多因子策略需要注入因子数据提供者和交易日历
	if cfg.StrategyName == "multi_factor" {
		if mf, ok := strat.(*strategy.MultiFactorStrategy); ok {
			// 创建因子数据提供者 (从 daily_basic / fina_indicator 等表获取因子数据)
			factorProvider := signal.NewFactorDataProvider(
				store.NewBasicRepo(db),
				store.NewFinaRepo(db),
				store.NewStockRepo(db),
				store.NewBarRepo(db),
			)
			mf.SetFactorDataProvider(factorProvider)
			// 设置交易日历 (用于判断调仓日)
			mf.SetCalendar(calendar)
			logger.L().Info("已为 multi_factor 策略注入因子数据提供者和交易日历")
		}
	}

	runID := fmt.Sprintf("bt_%s", time.Now().Format("20060102_150405"))

	return &Engine{
		cfg:          cfg,
		db:           db,
		barRepo:      store.NewBarRepo(db),
		calRepo:      calRepo,
		limitRepo:    limitRepo,
		tradeRepo:    store.NewTradeRepo(db),
		dataProvider: dataProvider,
		matcher:      matcher,
		account:      NewAccount(cfg.InitialCapital),
		strategy:     strat,
		calendar:     calendar,
		runID:        runID,
	}, nil
}

// Close 释放引擎持有的资源 (主要是数据库连接)
// 在批量回测 / 参数优化等场景下, 每次运行完毕后应调用, 避免连接泄漏
func (e *Engine) Close() error {
	if e.db != nil {
		return e.db.Close()
	}
	return nil
}

// Run 执行回测
func (e *Engine) Run() (*BacktestResult, error) {
	tradeDates := e.calendar.TradeDatesBetween(e.cfg.StartDate, e.cfg.EndDate)
	if len(tradeDates) == 0 {
		return nil, fmt.Errorf("回测区间内无交易日")
	}

	logger.L().Infof("开始回测: %s ~ %s, 交易日数: %d, 策略: %s, 资金: %.0f",
		e.cfg.StartDate, e.cfg.EndDate, len(tradeDates), e.strategy.Name(), e.cfg.InitialCapital)

	var allTrades []model.Trade
	var snapshots []model.AccountSnapshot

	for i, date := range tradeDates {
		// 1. 结算T+1 (开盘前)
		e.account.SettleT1()

		// 2. 构建当日行情
		bars := make(map[string]*model.Bar)
		for _, tsCode := range e.cfg.Universe {
			if bar := e.dataProvider.GetBar(tsCode, date); bar != nil {
				bars[tsCode] = bar
			}
		}

		// 3. 更新持仓市值
		e.account.UpdateMarketValue(bars)

		// 4. 构建策略上下文
		barCtx := &strategy.BarContext{
			TradeDate:  date,
			Universe:   e.cfg.Universe,
			Bars:       bars,
			Positions:  e.account.Positions,
			Cash:       e.account.Cash,
			TotalAsset: e.account.TotalAsset(),
			History:    e.dataProvider,
		}

		// 5. 策略产生信号
		signals, err := e.strategy.OnBar(context.Background(), barCtx)
		if err != nil {
			logger.L().Errorf("[%s] 策略执行出错: %v", date, err)
			continue
		}

		// 6. 撮合信号
		nextDate := ""
		if i+1 < len(tradeDates) {
			nextDate = tradeDates[i+1]
		}

		for _, signal := range signals {
			result := e.matcher.Match(signal, e.account, date, nextDate)
			LogFill(result, date)

			if result.Filled {
				// 记录成交
				trade := model.Trade{
					RunID:       e.runID,
					TsCode:      signal.TsCode,
					Side:        model.Side(signal.Direction),
					Price:       result.Price,
					Qty:         result.Qty,
					Amount:      result.Price * float64(result.Qty),
					Commission:  result.Cost.Commission,
					StampTax:    result.Cost.StampTax,
					TransferFee: result.Cost.TransferFee,
					TotalCost:   result.Cost.Total(),
					TradeDate:   date,
				}
				if result.Signal.Direction == model.DirBuy {
					trade.TradeTime = date + " 093000"
				} else {
					trade.TradeTime = date + " 093000"
				}
				e.tradeRepo.InsertTrade(&trade)
				allTrades = append(allTrades, trade)
			}
		}

		// 7. 更新市值 (成交后)
		e.account.UpdateMarketValue(bars)
		e.account.CleanEmptyPositions()

		// 8. 记录账户快照
		snap := e.account.Snapshot(date)
		e.account.LastTotalAsset = snap.TotalAsset
		snapshots = append(snapshots, snap)
		e.tradeRepo.InsertAccountSnapshot(e.runID, snap)

		if (i+1)%50 == 0 {
			logger.L().Infof("[%s] 进度: %d/%d, 总资产: %.2f, 现金: %.2f",
				date, i+1, len(tradeDates), snap.TotalAsset, snap.Cash)
		}
	}

	// 计算绩效
	var benchReturns []float64
	if e.cfg.Benchmark != "" {
		benchReturns = e.calcBenchmarkReturns(tradeDates)
	}

	metrics := CalculateMetrics(snapshots, allTrades, benchReturns)

	result := &BacktestResult{
		RunID:          e.runID,
		Metrics:        metrics,
		Snapshots:      snapshots,
		Trades:         allTrades,
		StrategyName:   e.strategy.Name(),
		Universe:       e.cfg.Universe,
		StartDate:      e.cfg.StartDate,
		EndDate:        e.cfg.EndDate,
		InitialCapital: e.cfg.InitialCapital,
	}

	e.printSummary(result)
	return result, nil
}

// calcBenchmarkReturns 计算基准日收益率序列
func (e *Engine) calcBenchmarkReturns(tradeDates []string) []float64 {
	bars, err := e.barRepo.GetBars(e.cfg.Benchmark, e.cfg.StartDate, e.cfg.EndDate)
	if err != nil || len(bars) < 2 {
		return nil
	}
	barMap := make(map[string]float64)
	for _, b := range bars {
		barMap[b.TradeDate] = b.Close
	}
	var returns []float64
	var prevClose float64
	for _, date := range tradeDates {
		close, ok := barMap[date]
		if !ok {
			continue
		}
		if prevClose > 0 {
			returns = append(returns, (close-prevClose)/prevClose)
		}
		prevClose = close
	}
	return returns
}

// printSummary 打印回测摘要
func (e *Engine) printSummary(result *BacktestResult) {
	// 静默模式: 跳过摘要打印 (参数优化等批量场景)
	if e.cfg.Silent {
		return
	}
	m := result.Metrics
	fmt.Println("\n========== 回测结果摘要 ==========")
	fmt.Printf("运行ID:     %s\n", result.RunID)
	fmt.Printf("策略:       %s\n", result.StrategyName)
	fmt.Printf("回测区间:   %s ~ %s\n", result.StartDate, result.EndDate)
	fmt.Printf("初始资金:   %.2f\n", result.InitialCapital)
	fmt.Printf("最终资产:   %.2f\n", result.Snapshots[len(result.Snapshots)-1].TotalAsset)
	fmt.Printf("总收益率:   %.2f%%\n", m.TotalReturn*100)
	fmt.Printf("年化收益:   %.2f%%\n", m.AnnualReturn*100)
	fmt.Printf("夏普比率:   %.2f\n", m.SharpeRatio)
	fmt.Printf("最大回撤:   %.2f%% (%s ~ %s)\n", m.MaxDrawdown*100, m.MaxDrawdownStart, m.MaxDrawdownEnd)
	fmt.Printf("交易次数:   %d (盈%d 亏%d)\n", m.TotalTrades, m.WinTrades, m.LossTrades)
	fmt.Printf("胜率:       %.2f%%\n", m.WinRate*100)
	fmt.Printf("盈亏比:     %.2f\n", m.ProfitLossRatio)
	if m.Beta != 0 {
		fmt.Printf("Alpha:      %.4f\n", m.Alpha)
		fmt.Printf("Beta:       %.4f\n", m.Beta)
	}
	fmt.Println("==================================")
}

// shiftDate 日期偏移 (粗略, 按天)
func shiftDate(dateStr string, days int) string {
	t, err := time.Parse("20060102", dateStr)
	if err != nil {
		return dateStr
	}
	t = t.AddDate(0, 0, days)
	return t.Format("20060102")
}

// BacktestResult 回测结果
type BacktestResult struct {
	RunID          string
	StrategyName   string
	Universe       []string
	StartDate      string
	EndDate        string
	InitialCapital float64
	Metrics        Metrics
	Snapshots      []model.AccountSnapshot
	Trades         []model.Trade
}
