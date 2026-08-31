package main

import (
	"flag"
	"fmt"

	"jingzhe-trader/internal/appcfg"
	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/engine"
	"jingzhe-trader/pkg/logger"
)

func main() {
	dbPath := flag.String("db", "", "数据库路径 (配置即存于此库, 默认取 "+appcfg.EnvDBPath+" 或 "+config.DefaultDBPath()+")")
	strategyName := flag.String("strategy", "ma_cross", "策略名称: ma_cross / macd / boll_breakout")
	startDate := flag.String("start", "", "回测起始日期 YYYYMMDD")
	endDate := flag.String("end", "", "回测结束日期 YYYYMMDD")
	universeStr := flag.String("universe", "", "股票池(逗号分隔, 缺省用配置 universe)")
	capital := flag.Float64("capital", 1000000, "初始资金")
	reportPath := flag.String("report", "reports/backtest_report.html", "报告输出路径")
	flag.Parse()

	// 先开库再读配置 (配置本体存在 config_kv 里)
	db, err := appcfg.Open(appcfg.ResolveDBPath(*dbPath))
	if err != nil {
		fmt.Printf("打开数据库失败: %v\n", err)
		return
	}
	defer db.Close()

	cfg, err := appcfg.Load(db)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		return
	}

	// 初始化日志
	logger.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.Output, cfg.Log.FilePath, cfg.Retention.LogDays)
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

	// 解析股票池 (缺省用配置 universe)
	universe := config.ParseUniverseCSV(*universeStr)
	if universe == nil {
		universe = cfg.UniverseCodes()
	}

	// 构建回测配置
	btCfg := engine.RunConfig{
		StartDate:      *startDate,
		EndDate:        *endDate,
		InitialCapital: *capital,
		Universe:       universe,
		Benchmark:      cfg.Backtest.Benchmark,
		Slippage:       cfg.Backtest.Slippage,
		FillPrice:      cfg.Backtest.FillPrice,
		StrategyName:   *strategyName,
		StrategyParams: cfg.StrategyParams(*strategyName),
	}

	fmt.Printf("=== 回测配置 ===\n")
	fmt.Printf("策略: %s\n", *strategyName)
	fmt.Printf("区间: %s ~ %s\n", *startDate, *endDate)
	fmt.Printf("资金: %.0f\n", *capital)
	fmt.Printf("股票池: %v\n", universe)
	fmt.Printf("================\n\n")

	// 创建并运行回测 (统一执行管道, 含风控); 数据库连接由本组合根持有
	runner, err := engine.NewBacktestRunner(db, btCfg, cfg)
	if err != nil {
		logger.L().Fatalf("创建回测运行器失败: %v", err)
	}

	result, err := runner.Run()
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
