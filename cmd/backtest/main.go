package main

import (
	"flag"
	"fmt"
	"strings"

	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/pkg/logger"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	strategyName := flag.String("strategy", "ma_cross", "策略名称: ma_cross / macd / boll_breakout")
	startDate := flag.String("start", "", "回测起始日期 YYYYMMDD")
	endDate := flag.String("end", "", "回测结束日期 YYYYMMDD")
	universeStr := flag.String("universe", "000001.SZ,600519.SH,000858.SZ,002415.SZ,600036.SH", "股票池(逗号分隔)")
	capital := flag.Float64("capital", 1000000, "初始资金")
	reportPath := flag.String("report", "reports/backtest_report.html", "报告输出路径")
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		return
	}

	// 初始化日志
	logger.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.Output, cfg.Log.FilePath)
	defer logger.Sync()

	// 使用配置默认值
	if *startDate == "" {
		*startDate = cfg.Backtest.StartDate
	}
	if *endDate == "" {
		*endDate = cfg.Backtest.EndDate
	}
	if *capital == 1000000 && cfg.Backtest.InitialCapital > 0 {
		*capital = cfg.Backtest.InitialCapital
	}

	// 解析股票池
	universe := strings.Split(*universeStr, ",")
	for i := range universe {
		universe[i] = strings.TrimSpace(universe[i])
	}

	// 构建回测配置
	btCfg := backtest.EngineConfig{
		StartDate:      *startDate,
		EndDate:        *endDate,
		InitialCapital: *capital,
		Universe:       universe,
		Benchmark:      cfg.Backtest.Benchmark,
		Slippage:       cfg.Backtest.Slippage,
		FillPrice:      cfg.Backtest.FillPrice,
		StrategyName:   *strategyName,
		StrategyParams: loadStrategyParams(cfg, *strategyName),
	}

	fmt.Printf("=== 回测配置 ===\n")
	fmt.Printf("策略: %s\n", *strategyName)
	fmt.Printf("区间: %s ~ %s\n", *startDate, *endDate)
	fmt.Printf("资金: %.0f\n", *capital)
	fmt.Printf("股票池: %v\n", universe)
	fmt.Printf("================\n\n")

	// 创建并运行回测引擎
	engine, err := backtest.NewEngine(btCfg, cfg)
	if err != nil {
		logger.L().Fatalf("创建回测引擎失败: %v", err)
	}

	result, err := engine.Run()
	if err != nil {
		logger.L().Fatalf("回测执行失败: %v", err)
	}

	// 生成HTML报告
	if err := backtest.GenerateHTMLReport(result, *reportPath); err != nil {
		logger.L().Errorf("生成报告失败: %v", err)
	} else {
		fmt.Printf("\n报告已生成: %s\n", *reportPath)
	}
}

// loadStrategyParams 从配置文件读取策略参数
func loadStrategyParams(cfg *config.Config, name string) map[string]interface{} {
	params := make(map[string]interface{})
	switch name {
	case "ma_cross":
		params["short_period"] = float64(cfg.Strategy.MACross.ShortPeriod)
		params["long_period"] = float64(cfg.Strategy.MACross.LongPeriod)
		params["position_pct"] = cfg.Strategy.MACross.PositionPct
	case "macd":
		params["fast"] = float64(cfg.Strategy.MACD.Fast)
		params["slow"] = float64(cfg.Strategy.MACD.Slow)
		params["signal"] = float64(cfg.Strategy.MACD.Signal)
		params["position_pct"] = cfg.Strategy.MACD.PositionPct
	case "multi_factor":
		params["position_pct"] = cfg.Strategy.MultiFactor.PositionPct
	default:
		params["position_pct"] = 0.15
	}
	return params
}
