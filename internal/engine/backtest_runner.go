package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// RunConfig 回测运行配置
type RunConfig struct {
	StartDate      string
	EndDate        string
	InitialCapital float64
	Universe       []string
	Benchmark      string
	Slippage       float64
	FillPrice      string // "next_open" 或 "close"
	StrategyName   string
	StrategyParams map[string]interface{}
	Silent         bool // 静默模式: 不打印回测摘要 (参数优化等批量场景)
}

// BacktestRunner 回测运行器
// 组装 Pipeline + PaperBroker, 回测与实盘走同一条执行管道 (含风控)
type BacktestRunner struct {
	cfg      RunConfig
	appCfg   *config.Config
	db       *sqlx.DB
	pipeline *Pipeline
	runID    string
}

// NewBacktestRunner 创建回测运行器
func NewBacktestRunner(cfg RunConfig, appCfg *config.Config) (*BacktestRunner, error) {
	db, err := store.NewDB(appCfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	calendar, dataProvider, validUniverse, err := loadBacktestData(db, cfg)
	if err != nil {
		db.Close()
		return nil, err
	}

	strat, err := buildStrategy(db, cfg, calendar)
	if err != nil {
		db.Close()
		return nil, err
	}

	// PaperBroker: 唯一模拟账户, 注入撮合规则 (滑点 + 涨跌停)
	costModel := market.NewCostModel(appCfg.Cost)
	pb := broker.NewPaperBroker("backtest", cfg.InitialCapital, costModel)
	pb.SetMatchRules(cfg.Slippage, store.NewLimitRepo(db))
	// 涨跌停检查口径: 前复权成交价换算回原始价比较
	pb.SetAdjRatioFn(dataProvider.AdjRatio)

	// 风控: 与实盘同一套配置, 保证回测结果反映真实约束
	rm := risk.NewRiskManager(appCfg.Risk)
	rm.SetSizeLimits(risk.SizeLimits{
		MinTradeAmount: appCfg.Trading.MinTradeAmount,
		MaxPositions:   appCfg.Trading.MaxPositions,
		MinCommission:  appCfg.Cost.MinCommission,
	})

	// 股票信息 (黑名单/ST过滤用)
	stocks := loadStocks(db, validUniverse)

	runID := fmt.Sprintf("bt_%s", time.Now().Format("20060102_150405"))
	pipeline := NewPipeline(PipelineConfig{
		Broker:    pb,
		Strategy:  strat,
		Risk:      rm,
		Data:      dataProvider,
		Calendar:  calendar,
		Universe:  validUniverse,
		StartDate: cfg.StartDate,
		EndDate:   cfg.EndDate,
		RunID:     runID,
		TradeRepo: store.NewTradeRepo(db),
		FillMode:  cfg.FillPrice,
		Stocks:    stocks,
	})

	return &BacktestRunner{cfg: cfg, appCfg: appCfg, db: db, pipeline: pipeline, runID: runID}, nil
}

// loadBacktestData 加载交易日历与行情数据, 过滤无数据股票
func loadBacktestData(db *sqlx.DB, cfg RunConfig) (*market.Calendar, *backtest.DataProvider, []string, error) {
	calRepo := store.NewCalendarRepo(db)
	cals, err := calRepo.GetTradeDays(cfg.StartDate, cfg.EndDate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("获取交易日历失败: %w", err)
	}
	if len(cals) == 0 {
		return nil, nil, nil, fmt.Errorf("回测区间内无交易日: %s ~ %s", cfg.StartDate, cfg.EndDate)
	}
	tradeDates := make([]string, len(cals))
	for i, c := range cals {
		tradeDates[i] = c.CalDate
	}
	calendar := market.NewCalendar(tradeDates)

	// 向前多加载1年历史数据用于指标计算
	preStartDate := shiftDate(cfg.StartDate, -365)
	dataProvider, err := backtest.NewDataProvider(store.NewBarRepo(db), cfg.Universe, preStartDate, cfg.EndDate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("加载数据失败: %w", err)
	}

	validUniverse := make([]string, 0, len(cfg.Universe))
	for _, code := range cfg.Universe {
		if dataProvider.HasData(code) {
			validUniverse = append(validUniverse, code)
		}
	}
	if len(validUniverse) == 0 {
		return nil, nil, nil, fmt.Errorf("股票池中无有效数据")
	}
	logger.L().Infof("有效股票池: %d/%d", len(validUniverse), len(cfg.Universe))
	return calendar, dataProvider, validUniverse, nil
}

// buildStrategy 从注册表构建策略并注入依赖
func buildStrategy(db *sqlx.DB, cfg RunConfig, calendar *market.Calendar) (strategy.Strategy, error) {
	registry := strategy.DefaultRegistry()
	strat, ok := registry.Get(cfg.StrategyName)
	if !ok {
		return nil, fmt.Errorf("未知策略: %s, 可用策略: %v", cfg.StrategyName, registry.Names())
	}
	if err := strat.Init(context.Background(), cfg.StrategyParams); err != nil {
		return nil, fmt.Errorf("策略初始化失败: %w", err)
	}

	// 多因子策略需要注入因子数据提供者和交易日历
	if mf, ok := strat.(*strategy.MultiFactorStrategy); ok {
		factorProvider := signal.NewFactorDataProvider(
			store.NewBasicRepo(db),
			store.NewFinaRepo(db),
			store.NewStockRepo(db),
			store.NewBarRepo(db),
		)
		mf.SetFactorDataProvider(factorProvider)
		mf.SetCalendar(calendar)
		logger.L().Info("已为 multi_factor 策略注入因子数据提供者和交易日历")
	}
	return strat, nil
}

// loadStocks 加载股票基本信息 (风控黑名单/ST过滤用)
func loadStocks(db *sqlx.DB, universe []string) map[string]*model.Stock {
	stocks := make(map[string]*model.Stock, len(universe))
	repo := store.NewStockRepo(db)
	for _, code := range universe {
		if s, err := repo.GetByCode(code); err == nil && s != nil {
			stocks[code] = s
		} else {
			stocks[code] = &model.Stock{TsCode: code, ListStatus: "L"}
		}
	}
	return stocks
}

// Close 释放运行器持有的资源 (数据库连接)
// 批量回测/参数优化场景每次运行完毕后必须调用, 避免连接泄漏
func (r *BacktestRunner) Close() error {
	if r.db != nil {
		return r.db.Close()
	}
	return nil
}

// Run 执行回测并计算绩效
func (r *BacktestRunner) Run() (*backtest.BacktestResult, error) {
	if err := r.pipeline.Run(); err != nil {
		return nil, fmt.Errorf("回测执行失败: %w", err)
	}

	snapshots := r.pipeline.Snapshots()
	trades := r.pipeline.Trades()

	// 策略执行失败天数显著告警: 有失败日的回测结果不可信 (那些天只有止损信号在交易)
	if days := r.pipeline.StrategyErrorDays(); days > 0 {
		logger.L().Warnf("⚠️⚠️ 策略在 %d 个交易日执行失败, 本次回测结果不可信, 请先修复策略错误 ⚠️⚠️", days)
	}

	var benchReturns []float64
	var benchSnapshots []backtest.BenchmarkSnapshot
	if r.cfg.Benchmark != "" {
		benchReturns, benchSnapshots = r.calcBenchmarkReturns()
	}
	metrics := backtest.CalculateMetrics(snapshots, trades, benchReturns)

	result := &backtest.BacktestResult{
		RunID:              r.runID,
		Metrics:            metrics,
		Snapshots:          snapshots,
		Trades:             trades,
		StrategyName:       r.pipeline.cfg.Strategy.Name(),
		Universe:           r.pipeline.cfg.Universe,
		StartDate:          r.cfg.StartDate,
		EndDate:            r.cfg.EndDate,
		InitialCapital:     r.cfg.InitialCapital,
		BenchmarkName:      r.cfg.Benchmark,
		BenchmarkSnapshots: benchSnapshots,
	}

	r.printSummary(result)
	return result, nil
}

// calcBenchmarkReturns 计算基准日收益率序列, 并顺带产出逐日基准净值 (归一化, 起点=1)
func (r *BacktestRunner) calcBenchmarkReturns() ([]float64, []backtest.BenchmarkSnapshot) {
	barRepo := store.NewBarRepo(r.db)
	bars, err := barRepo.GetBars(r.cfg.Benchmark, r.cfg.StartDate, r.cfg.EndDate)
	if err != nil || len(bars) < 2 {
		return nil, nil
	}
	var returns []float64
	snapshots := make([]backtest.BenchmarkSnapshot, 0, len(bars))
	var prevClose, firstClose float64
	for _, b := range bars {
		if firstClose == 0 {
			firstClose = b.Close
		}
		if prevClose > 0 {
			returns = append(returns, (b.Close-prevClose)/prevClose)
		}
		prevClose = b.Close
		nav := 1.0
		if firstClose > 0 {
			nav = b.Close / firstClose
		}
		snapshots = append(snapshots, backtest.BenchmarkSnapshot{TradeDate: b.TradeDate, Nav: nav})
	}
	return returns, snapshots
}

// printSummary 打印回测摘要
func (r *BacktestRunner) printSummary(result *backtest.BacktestResult) {
	if r.cfg.Silent || len(result.Snapshots) == 0 {
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

// shiftDate 日期偏移 (按天)
func shiftDate(dateStr string, days int) string {
	t, err := time.Parse("20060102", dateStr)
	if err != nil {
		return dateStr
	}
	return t.AddDate(0, 0, days).Format("20060102")
}
