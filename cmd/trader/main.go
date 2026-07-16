package main

import (
	"flag"
	"fmt"
	"strings"

	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/internal/trading"
	"jingzhe-trader/pkg/logger"
)

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	strategyName := flag.String("strategy", "ma_cross", "策略名称")
	startDate := flag.String("start", "", "起始日期 YYYYMMDD")
	endDate := flag.String("end", "", "结束日期 YYYYMMDD")
	universeStr := flag.String("universe", "000001.SZ,600519.SH,000858.SZ,002415.SZ,600036.SH", "股票池")
	capital := flag.Float64("capital", 1000000, "初始资金")
	reportPath := flag.String("report", "reports/trader_report.html", "报告路径")

	// 新增 broker 切换参数
	brokerType := flag.String("broker", "paper", "券商类型: paper/qmt")
	qmtPath := flag.String("qmt-path", "", "miniQMT userdata_mini 路径 (qmt模式必需)")
	qmtAccount := flag.String("qmt-account", "", "资金账号 (qmt模式必需)")
	qmtSession := flag.Int("qmt-session", 123456, "session_id")
	qmtURL := flag.String("qmt-url", "http://127.0.0.1:16888", "sidecar URL")

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

	if *startDate == "" {
		*startDate = cfg.Backtest.StartDate
	}
	if *endDate == "" {
		*endDate = cfg.Backtest.EndDate
	}

	universe := strings.Split(*universeStr, ",")
	for i := range universe {
		universe[i] = strings.TrimSpace(universe[i])
	}

	// 初始化数据库
	db, err := store.NewDB(cfg.Database.Path)
	if err != nil {
		logger.L().Fatalf("初始化数据库失败: %v", err)
	}
	defer db.Close()

	// 获取交易日历
	calRepo := store.NewCalendarRepo(db)
	cals, err := calRepo.GetTradeDays(*startDate, *endDate)
	if err != nil {
		logger.L().Fatalf("获取交易日历失败: %v", err)
	}
	if len(cals) == 0 {
		logger.L().Fatalf("区间内无交易日")
	}
	tradeDates := make([]string, len(cals))
	for i, c := range cals {
		tradeDates[i] = c.CalDate
	}
	calendar := market.NewCalendar(tradeDates)

	// 预加载数据（策略指标计算需要历史数据）
	preStartDate := shiftDate(*startDate, -365)
	dataProvider, err := backtest.NewDataProvider(store.NewBarRepo(db), universe, preStartDate, *endDate)
	if err != nil {
		logger.L().Fatalf("加载数据失败: %v", err)
	}
	validUniverse := make([]string, 0, len(universe))
	checkDate := *startDate
	if len(tradeDates) > 0 {
		checkDate = tradeDates[0] // 使用第一个交易日检查
	}
	for _, code := range universe {
		if dataProvider.GetBar(code, checkDate) != nil {
			validUniverse = append(validUniverse, code)
		}
	}
	logger.L().Infof("有效股票池: %d/%d (检查日期: %s)", len(validUniverse), len(universe), checkDate)

	// 创建策略
	registry := strategy.DefaultRegistry()
	strat, ok := registry.Get(*strategyName)
	if !ok {
		logger.L().Fatalf("未知策略: %s, 可用: %v", *strategyName, registry.Names())
	}
	strat.Init(nil, map[string]interface{}{"position_pct": 0.15})

	// 多因子策略需要注入因子数据提供者和交易日历
	if *strategyName == "multi_factor" {
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

	// 创建风控
	riskManager := risk.NewRiskManager(cfg.Risk)

	// 根据 broker 类型创建实例
	var br broker.Broker
	var paperBroker *broker.PaperBroker

	switch *brokerType {
	case "qmt":
		if *qmtPath == "" || *qmtAccount == "" {
			logger.L().Fatalf("qmt 模式需要指定 --qmt-path 和 --qmt-account")
		}

		qmtBridge := broker.NewQMTBridge(*qmtURL)

		// 健康检查
		healthy, err := qmtBridge.Health()
		if err != nil || !healthy {
			logger.L().Fatalf("QMT sidecar 连接失败: %v", err)
		}
		logger.L().Infof("QMT sidecar 健康检查通过: %s", *qmtURL)

		// 连接 miniQMT
		if err := qmtBridge.Connect(*qmtPath, *qmtSession, *qmtAccount); err != nil {
			logger.L().Fatalf("QMT 连接失败: %v", err)
		}

		// 查询并打印初始持仓和资产
		asset, err := qmtBridge.QueryAsset()
		if err != nil {
			logger.L().Warnf("查询 QMT 初始资产失败: %v", err)
		} else {
			logger.L().Infof("QMT 初始资产: 现金=%.2f 总资产=%.2f 市值=%.2f 持仓数=%d",
				asset.Cash, asset.TotalAsset, asset.MarketValue, len(asset.Positions))
		}

		br = qmtBridge
		logger.L().Infof("使用 QMT 实盘券商: %s", *qmtURL)

	default:
		// paper 模式（默认）
		costModel := market.NewCostModel(cfg.Cost)
		paperBroker = broker.NewPaperBroker("paper", *capital, costModel)
		br = paperBroker
		logger.L().Infof("使用 PaperBroker 纸面交易")
	}

	// 创建交易循环
	loop := trading.NewLoop(
		br,
		strat,
		riskManager,
		dataProvider,
		calendar,
		validUniverse,
		*startDate,
		*endDate,
	)

	fmt.Printf("=== 交易配置 ===\n")
	fmt.Printf("Broker: %s\n", br.Name())
	fmt.Printf("策略: %s\n", *strategyName)
	fmt.Printf("区间: %s ~ %s\n", *startDate, *endDate)
	if *brokerType != "qmt" {
		fmt.Printf("资金: %.0f\n", *capital)
	}
	fmt.Printf("==================\n\n")

	// 运行
	if err := loop.Run(); err != nil {
		logger.L().Fatalf("交易循环失败: %v", err)
	}

	// 打印摘要
	snapshots := loop.Snapshots()
	trades := loop.Trades()
	if len(snapshots) > 0 {
		last := snapshots[len(snapshots)-1]
		first := snapshots[0]
		fmt.Println("\n========== 交易结果 ==========")
		fmt.Printf("初始资金:   %.2f\n", first.TotalAsset)
		fmt.Printf("最终资产:   %.2f\n", last.TotalAsset)
		fmt.Printf("总收益率:   %.2f%%\n", (last.TotalAsset-first.TotalAsset)/first.TotalAsset*100)
		fmt.Printf("交易笔数:   %d\n", len(trades))
		fmt.Println("==================================")
	}

	// PaperBroker 特有的 OMS 统计
	if paperBroker != nil {
		total, filled, canceled, rejected := paperBroker.GetOMS().Stats()
		fmt.Printf("\n订单统计: 总%d 成交%d 撤单%d 拒绝%d\n", total, filled, canceled, rejected)
	}

	// 生成报告
	result := &backtest.BacktestResult{
		RunID:          fmt.Sprintf("trader_%s_%s", *strategyName, *brokerType),
		StrategyName:   *strategyName,
		Universe:       validUniverse,
		StartDate:      *startDate,
		EndDate:        *endDate,
		InitialCapital: *capital,
		Snapshots:      snapshots,
		Trades:         trades,
	}
	if err := backtest.GenerateHTMLReport(result, *reportPath); err != nil {
		logger.L().Errorf("生成报告失败: %v", err)
	} else {
		fmt.Printf("报告已生成: %s\n", *reportPath)
	}
}

func shiftDate(dateStr string, days int) string {
	if len(dateStr) != 8 {
		return dateStr
	}
	year := int(dateStr[0]-'0')*1000 + int(dateStr[1]-'0')*100 + int(dateStr[2]-'0')*10 + int(dateStr[3]-'0')
	month := int(dateStr[4]-'0')*10 + int(dateStr[5]-'0')
	day := int(dateStr[6]-'0')*10 + int(dateStr[7]-'0')

	// 简化处理, 每月按30天
	totalDays := year*365 + month*30 + day + days
	y := totalDays / 365
	rem := totalDays % 365
	m := rem / 30
	if m < 1 {
		m = 1
	}
	if m > 12 {
		m = 12
	}
	d := rem % 30
	if d < 1 {
		d = 1
	}
	return fmt.Sprintf("%04d%02d%02d", y, m, d)
}
