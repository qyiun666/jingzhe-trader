package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/factor"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// SignalOutput 信号输出结构 (用于JSON文件输出)
type SignalOutput struct {
	Date         string          `json:"date"`
	Strategy     string          `json:"strategy"`
	SignalCount  int             `json:"signal_count"`
	Signals      []SignalRecord  `json:"signals"`
}

// SignalRecord 单条信号记录
type SignalRecord struct {
	TsCode    string  `json:"ts_code"`
	Direction string  `json:"direction"` // buy / sell / hold
	TargetQty int     `json:"target_qty"`
	Reason    string  `json:"reason"`
	Strength  float64 `json:"strength"`
}

func main() {
	// 解析命令行参数
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	strategyName := flag.String("strategy", "multi_factor", "策略名称")
	mode := flag.String("mode", "daily", "运行模式: batch / daily")
	date := flag.String("date", "", "单日期 (daily模式, YYYYMMDD)")
	startDate := flag.String("start", "", "起始日期 (batch模式, YYYYMMDD)")
	endDate := flag.String("end", "", "结束日期 (batch模式, YYYYMMDD)")
	universeStr := flag.String("universe", "", "股票池 (逗号分隔, 为空则使用全市场)")
	outputPath := flag.String("output", "signals.json", "输出文件路径")
	initialCapital := flag.Float64("capital", 1000000, "初始资金 (用于计算买入数量)")
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(1)
	}

	// 初始化日志
	logger.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.Output, cfg.Log.FilePath)
	defer logger.Sync()

	// 参数校验
	if *mode != "batch" && *mode != "daily" {
		fmt.Printf("无效的运行模式: %s, 请使用 batch 或 daily\n", *mode)
		os.Exit(1)
	}
	if *mode == "daily" && *date == "" {
		fmt.Println("daily模式需要指定 -date 参数")
		os.Exit(1)
	}
	if *mode == "batch" && (*startDate == "" || *endDate == "") {
		fmt.Println("batch模式需要指定 -start 和 -end 参数")
		os.Exit(1)
	}

	// 解析股票池
	var universe []string
	if *universeStr != "" {
		universe = strings.Split(*universeStr, ",")
		for i := range universe {
			universe[i] = strings.TrimSpace(universe[i])
		}
	}

	// 初始化数据库
	db, err := store.NewDB(cfg.Database.Path)
	if err != nil {
		logger.L().Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	// 初始化仓储
	basicRepo := store.NewBasicRepo(db)
	finRepo := store.NewFinaRepo(db)
	stockRepo := store.NewStockRepo(db)
	barRepo := store.NewBarRepo(db)
	calRepo := store.NewCalendarRepo(db)

	// 初始化因子数据提供者
	factorProvider := signal.NewFactorDataProvider(basicRepo, finRepo, stockRepo, barRepo)

	// 如果未指定股票池, 使用全市场已上市股票
	if len(universe) == 0 {
		allStocks, err := stockRepo.GetAll()
		if err != nil {
			logger.L().Fatalf("获取全市场股票失败: %v", err)
		}
		for _, s := range allStocks {
			if s.ListStatus == "L" && !s.IsST {
				universe = append(universe, s.TsCode)
			}
		}
		logger.L().Infof("使用全市场股票池: %d 只", len(universe))
	}

	// 初始化策略
	registry := strategy.DefaultRegistry()
	strat, ok := registry.Get(*strategyName)
	if !ok {
		logger.L().Fatalf("未知策略: %s, 可用策略: %v", *strategyName, registry.Names())
	}

	// 策略参数
	strategyParams := map[string]interface{}{
		"top_n":            20,
		"rebalance_freq":   "monthly",
		"position_pct":     0.05,
		"stop_loss_pct":    -0.15,
		"take_profit_pct":  0.30,
		"momentum_period":  60,
	}
	if err := strat.Init(context.Background(), strategyParams); err != nil {
		logger.L().Fatalf("策略初始化失败: %v", err)
	}

	// 如果是多因子策略, 注入因子数据提供者和日历
	if mfStrat, ok := strat.(*strategy.MultiFactorStrategy); ok {
		mfStrat.SetFactorDataProvider(factorProvider)

		// 加载交易日历
		cals, err := calRepo.GetTradeDays("20200101", "20301231")
		if err != nil {
			logger.L().Fatalf("获取交易日历失败: %v", err)
		}
		tradeDates := make([]string, len(cals))
		for i, c := range cals {
			tradeDates[i] = c.CalDate
		}
		cal := market.NewCalendar(tradeDates)
		mfStrat.SetCalendar(cal)
	}

	// 根据模式运行
	ctx := context.Background()
	var allOutputs []SignalOutput

	switch *mode {
	case "daily":
		output, err := runDaily(ctx, strat, factorProvider, barRepo, calRepo, universe, *date, *initialCapital)
		if err != nil {
			logger.L().Fatalf("生成当日信号失败: %v", err)
		}
		allOutputs = append(allOutputs, *output)

	case "batch":
		outputs, err := runBatch(ctx, strat, factorProvider, barRepo, calRepo, universe, *startDate, *endDate, *initialCapital)
		if err != nil {
			logger.L().Fatalf("批量生成信号失败: %v", err)
		}
		allOutputs = outputs
	}

	// 打印结果
	printSignals(allOutputs)

	// 保存到文件
	if err := saveSignals(allOutputs, *outputPath); err != nil {
		logger.L().Errorf("保存信号文件失败: %v", err)
	} else {
		fmt.Printf("\n信号已保存到: %s\n", *outputPath)
	}
}

// runDaily 生成单日信号
func runDaily(
	ctx context.Context,
	strat strategy.Strategy,
	factorProvider factor.DataProvider,
	barRepo *store.BarRepo,
	calRepo *store.CalendarRepo,
	universe []string,
	date string,
	capital float64,
) (*SignalOutput, error) {
	logger.L().Infof("生成 %s 的信号, 策略: %s, 股票池: %d 只", date, strat.Name(), len(universe))

	// 获取当日行情
	bars := make(map[string]*model.Bar)
	for _, tsCode := range universe {
		// 获取当日及之前的少量数据 (用于确认当日有数据)
		stockBars, err := barRepo.GetBars(tsCode, date, date)
		if err != nil || len(stockBars) == 0 {
			continue
		}
		b := stockBars[0]
		bars[tsCode] = &b
	}

	if len(bars) == 0 {
		return nil, fmt.Errorf("日期 %s 无行情数据", date)
	}

	// 构建策略上下文
	barCtx := &strategy.BarContext{
		TradeDate:  date,
		Universe:   universe,
		Bars:       bars,
		Positions:  make(map[string]*model.Position),
		Cash:       capital,
		TotalAsset: capital,
		History:    &signalHistoryProvider{barRepo: barRepo},
	}

	// 执行策略
	signals, err := strat.OnBar(ctx, barCtx)
	if err != nil {
		return nil, fmt.Errorf("策略执行失败: %w", err)
	}

	// 转换为输出格式
	records := make([]SignalRecord, 0, len(signals))
	for _, s := range signals {
		dirStr := "hold"
		switch s.Direction {
		case model.DirBuy:
			dirStr = "buy"
		case model.DirSell:
			dirStr = "sell"
		}
		records = append(records, SignalRecord{
			TsCode:    s.TsCode,
			Direction: dirStr,
			TargetQty: s.TargetQty,
			Reason:    s.Reason,
			Strength:  s.Strength,
		})
	}

	return &SignalOutput{
		Date:        date,
		Strategy:    strat.Name(),
		SignalCount: len(records),
		Signals:     records,
	}, nil
}

// runBatch 批量生成历史信号
func runBatch(
	ctx context.Context,
	strat strategy.Strategy,
	factorProvider factor.DataProvider,
	barRepo *store.BarRepo,
	calRepo *store.CalendarRepo,
	universe []string,
	startDate, endDate string,
	capital float64,
) ([]SignalOutput, error) {
	logger.L().Infof("批量生成信号: %s ~ %s, 策略: %s", startDate, endDate, strat.Name())

	// 获取区间内的交易日
	cals, err := calRepo.GetTradeDays(startDate, endDate)
	if err != nil {
		return nil, fmt.Errorf("获取交易日历失败: %w", err)
	}
	if len(cals) == 0 {
		return nil, fmt.Errorf("区间内无交易日: %s ~ %s", startDate, endDate)
	}

	var outputs []SignalOutput
	for i, cal := range cals {
		output, err := runDaily(ctx, strat, factorProvider, barRepo, calRepo, universe, cal.CalDate, capital)
		if err != nil {
			logger.L().Warnf("[%s] 生成信号失败: %v", cal.CalDate, err)
			continue
		}
		outputs = append(outputs, *output)

		if (i+1)%10 == 0 {
			logger.L().Infof("进度: %d/%d", i+1, len(cals))
		}
	}

	return outputs, nil
}

// printSignals 打印信号列表
func printSignals(outputs []SignalOutput) {
	fmt.Println("\n========== 信号列表 ==========")
	totalSignals := 0
	for _, o := range outputs {
		fmt.Printf("\n日期: %s, 策略: %s, 信号数: %d\n", o.Date, o.Strategy, o.SignalCount)
		for _, s := range o.Signals {
			fmt.Printf("  %s  %-4s  qty=%-6d  strength=%.2f  %s\n",
				s.TsCode, s.Direction, s.TargetQty, s.Strength, s.Reason)
		}
		totalSignals += o.SignalCount
	}
	fmt.Printf("\n总计: %d 个交易日, %d 条信号\n", len(outputs), totalSignals)
	fmt.Println("==============================")
}

// saveSignals 保存信号到JSON文件
func saveSignals(outputs []SignalOutput, outputPath string) error {
	// 确保目录存在
	dir := filepath.Dir(outputPath)
	if dir != "" && dir != "." {
		os.MkdirAll(dir, 0755)
	}

	data, err := json.MarshalIndent(outputs, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化信号失败: %w", err)
	}

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	return nil
}

// signalHistoryProvider 信号引擎的历史数据提供者实现
// 简单封装 barRepo, 用于策略的 HistoryProvider 接口
type signalHistoryProvider struct {
	barRepo *store.BarRepo
}

// GetBars 获取指定股票截至 endDate 的 N 根日线
func (h *signalHistoryProvider) GetBars(tsCode, endDate string, n int) ([]model.Bar, error) {
	// 往前推 n*2 天, 确保有足够的交易日
	startDate := shiftDateStr(endDate, -n*2)
	bars, err := h.barRepo.GetBars(tsCode, startDate, endDate)
	if err != nil {
		return nil, err
	}
	if len(bars) <= n {
		return bars, nil
	}
	return bars[len(bars)-n:], nil
}

// GetCloses 获取指定股票截至 endDate 的 N 个收盘价
func (h *signalHistoryProvider) GetCloses(tsCode, endDate string, n int) ([]float64, error) {
	bars, err := h.GetBars(tsCode, endDate, n)
	if err != nil {
		return nil, err
	}
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.AdjClose()
	}
	return closes, nil
}

// shiftDateStr 日期字符串偏移 (YYYYMMDD 格式)
func shiftDateStr(dateStr string, days int) string {
	t, err := time.Parse("20060102", dateStr)
	if err != nil {
		return dateStr
	}
	t = t.AddDate(0, 0, days)
	return t.Format("20060102")
}
