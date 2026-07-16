package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/report"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"

	"strconv"
)

var (
	modeFlag     = flag.String("mode", "daily", "运行模式: daily/review/monitor/diagnose/rebalance")
	dateFlag     = flag.String("date", "", "日期 YYYYMMDD (默认今天)")
	configFlag   = flag.String("config", "", "配置文件路径")
	brokerFlag   = flag.String("broker", "paper", "券商类型: paper/qmt")
	strategyFlag = flag.String("strategy", "ma_cross", "使用的策略名")
	reportFlag   = flag.String("report", "", "报告输出路径")
	universeFlag = flag.String("universe", "", "股票池文件路径")
)

// saveSnapshot 保存账户快照到数据库
func saveSnapshot(db *sqlx.DB, tradeDate string, asset *broker.AssetInfo, positions map[string]*model.Position) error {
	if asset == nil {
		return fmt.Errorf("asset is nil")
	}

	// 计算市值
	marketValue := asset.TotalAsset - asset.Cash

	// 计算累计盈亏（需要 initial_capital）
	var totalPnL, totalPnLPct float64
	portfolioRepo := store.NewPortfolioRepo(db)
	initialCapitalStr, _ := portfolioRepo.GetMeta("initial_capital")
	if initialCapitalStr != "" {
		var initialCapital float64
		fmt.Sscanf(initialCapitalStr, "%f", &initialCapital)
		if initialCapital > 0 {
			totalPnL = asset.TotalAsset - initialCapital
			totalPnLPct = totalPnL / initialCapital
		}
	}

	// 查询前一日快照计算当日盈亏
	var pnl, pnlPct float64
	var prevSnap model.AccountSnapshot
	err := db.Get(&prevSnap, "SELECT * FROM account_snapshot ORDER BY trade_date DESC LIMIT 1")
	if err == nil && prevSnap.TotalAsset > 0 {
		pnl = asset.TotalAsset - prevSnap.TotalAsset
		pnlPct = pnl / prevSnap.TotalAsset
	}

	_, err = db.Exec(`INSERT INTO account_snapshot
		(trade_date, total_asset, cash, market_value, pnl, pnl_pct, total_pnl, total_pnl_pct)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trade_date) DO UPDATE SET
			total_asset = excluded.total_asset,
			cash = excluded.cash,
			market_value = excluded.market_value,
			pnl = excluded.pnl,
			pnl_pct = excluded.pnl_pct,
			total_pnl = excluded.total_pnl,
			total_pnl_pct = excluded.total_pnl_pct`,
		tradeDate, asset.TotalAsset, asset.Cash, marketValue, pnl, pnlPct, totalPnL, totalPnLPct)
	if err != nil {
		return fmt.Errorf("插入快照失败: %w", err)
	}
	logger.L().Infof("账户快照已保存: %s 总资产=%.2f", tradeDate, asset.TotalAsset)
	return nil
}

func main() {
	flag.Parse()

	logger.L().Infof("[Captain] 启动 mode=%s date=%s", *modeFlag, *dateFlag)

	// 确定交易日期
	tradeDate := *dateFlag
	if tradeDate == "" {
		tradeDate = time.Now().Format("20060102")
	}

	// 加载配置
	cfgPath := *configFlag
	if cfgPath == "" {
		cfgPath = config.DefaultConfigPath()
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		logger.L().Errorf("加载配置失败: %v", err)
		// 使用默认配置继续
		cfg = &config.Config{}
	}

	// 初始化数据库
	db, err := store.NewDB(cfg.Database.Path)
	if err != nil {
		logger.L().Errorf("初始化数据库失败: %v", err)
		os.Exit(1)
	}
	defer db.Close()

	ctx := context.Background()

	switch *modeFlag {
	case "daily":
		if err := runDaily(ctx, cfg, db, tradeDate); err != nil {
			logger.L().Errorf("daily 模式执行失败: %v", err)
			os.Exit(1)
		}
	case "review":
		if err := runReview(ctx, cfg, db, tradeDate); err != nil {
			logger.L().Errorf("review 模式执行失败: %v", err)
			os.Exit(1)
		}
	case "monitor":
		if err := runMonitor(ctx, cfg, db, tradeDate); err != nil {
			logger.L().Errorf("monitor 模式执行失败: %v", err)
			os.Exit(1)
		}
	case "diagnose":
		if err := runDiagnose(ctx, cfg, db, tradeDate); err != nil {
			logger.L().Errorf("diagnose 模式执行失败: %v", err)
			os.Exit(1)
		}
	case "rebalance":
		if err := runRebalance(ctx, cfg, db, tradeDate); err != nil {
			logger.L().Errorf("rebalance 模式执行失败: %v", err)
			os.Exit(1)
		}
	default:
		logger.L().Errorf("未知模式: %s", *modeFlag)
		flag.Usage()
		os.Exit(1)
	}
}

// runDaily 生成每日操盘报告
func runDaily(ctx context.Context, cfg *config.Config, db *sqlx.DB, tradeDate string) error {
	logger.L().Infof("[Daily] 开始生成 %s 操盘报告", tradeDate)

	// 1. 初始化仓储
	barRepo := store.NewBarRepo(db)
	calRepo := store.NewCalendarRepo(db)
	stockRepo := store.NewStockRepo(db)

	// 2. 校验交易日
	isTradeDay, err := calRepo.IsTradeDay(tradeDate)
	if err != nil {
		return fmt.Errorf("查询交易日历失败: %w", err)
	}
	if !isTradeDay {
		logger.L().Warnf("%s 非交易日, 报告将使用最近可用数据", tradeDate)
	}

	// 3. 获取上一交易日
	preTradeDate, err := calRepo.GetPreTradeDate(tradeDate)
	if err != nil {
		logger.L().Warnf("获取上一交易日失败: %v", err)
	}

	// 4. 获取当日全市场行情
	allBars, err := barRepo.GetBarsByDate(tradeDate)
	if err != nil {
		return fmt.Errorf("获取当日行情失败: %w", err)
	}
	if len(allBars) == 0 {
		return fmt.Errorf("当日 %s 无行情数据", tradeDate)
	}

	// 5. 获取上一日行情 (用于计算量比和涨跌对比)
	prevBars := make(map[string]*model.Bar)
	if preTradeDate != "" {
		prevBarsList, err := barRepo.GetBarsByDate(preTradeDate)
		if err == nil {
			for i := range prevBarsList {
				b := &prevBarsList[i]
				prevBars[b.TsCode] = b
			}
		}
	}

	// 6. 初始化券商并获取持仓
	brk := buildBroker(cfg, db)
	positions, err := brk.QueryPositions()
	if err != nil {
		logger.L().Warnf("查询持仓失败: %v", err)
		positions = make(map[string]*model.Position)
	}
	asset, err := brk.QueryAsset()
	if err != nil {
		logger.L().Warnf("查询资产失败: %v", err)
		asset = &broker.AssetInfo{Cash: cfg.Backtest.InitialCapital}
	}

	// 更新持仓市值
	todayBars := make(map[string]*model.Bar)
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}
	brk.UpdateMarketValue(todayBars)
	positions, _ = brk.QueryPositions() // 重新查询更新后的持仓

	// 7. 获取股票名称映射
	stockMap := buildStockMap(stockRepo)

	// 8. 运行策略产生信号
	signals := runStrategy(ctx, cfg, *strategyFlag, todayBars, positions, asset)

	// 9. 构建持仓分析 (report 包需要的展示结构)
	portfolioSummary := buildPortfolioSummary(positions, asset, stockMap)

	// 10. 构建调仓计划
	rebalanceSummary := buildRebalanceSummary(signals, positions, asset)

	// 11. 构建策略建议
	strategySummary := buildStrategySummary(*strategyFlag, signals, portfolioSummary)

	// 12. 监控市场状态
	moneyflows := []analysis.MoneyFlow{} // 占位
	toplists := []analysis.TopList{}     // 占位
	marketSnapshot := analysis.MonitorMarket(tradeDate, allBars, prevBars, moneyflows, toplists)
	analysis.SetPrevBars(prevBars)
	targetBuys := extractBuyTargets(signals)
	marketSnapshot.MonitorPortfolio(positions, todayBars, targetBuys)

	// 13. 新闻摘要 (占位, 后续接入真实新闻分析)
	newsSummary := &report.NewsSummary{
		TradeDate:     tradeDate,
		Items:         []report.NewsItem{},
		Sentiment:     "neutral",
		RelatedStocks: make(map[string][]report.NewsItem),
	}

	// 14. 构建操作清单
	actionItems := buildActionItems(signals, portfolioSummary, marketSnapshot)

	// 15. 组装报告
	dailyReport := &report.DailyReport{
		Date:              tradeDate,
		MarketSnapshot:    marketSnapshot,
		PortfolioAnalysis: portfolioSummary,
		RebalancePlan:     rebalanceSummary,
		StrategyAdvice:    strategySummary,
		NewsSummary:       newsSummary,
		ActionItems:       actionItems,
	}

	// 16. 输出报告
	outputPath := *reportFlag
	if outputPath == "" {
		outputPath = fmt.Sprintf("reports/daily_report_%s.html", tradeDate)
	}
	if err := report.GenerateDailyReport(dailyReport, outputPath); err != nil {
		return fmt.Errorf("生成报告失败: %w", err)
	}

	logger.L().Infof("[Daily] 报告已生成: %s", outputPath)

	// 17. 保存账户快照
	if err := saveSnapshot(db, tradeDate, asset, positions); err != nil {
		logger.L().Warnf("保存账户快照失败: %v", err)
	}

	return nil
}

// runReview 盘后复盘报告
func runReview(ctx context.Context, cfg *config.Config, db *sqlx.DB, tradeDate string) error {
	logger.L().Infof("[Review] 开始生成 %s 盘后复盘", tradeDate)
	// 复用 daily 逻辑, 仅报告标题不同
	return runDaily(ctx, cfg, db, tradeDate)
}

// runMonitor 实时监控模式 (模拟盘中盯盘)
func runMonitor(ctx context.Context, cfg *config.Config, db *sqlx.DB, tradeDate string) error {
	logger.L().Infof("[Monitor] 启动实时监控 (交易日期: %s)", tradeDate)
	barRepo := store.NewBarRepo(db)
	calRepo := store.NewCalendarRepo(db)

	preTradeDate, _ := calRepo.GetPreTradeDate(tradeDate)
	prevBars := make(map[string]*model.Bar)
	if preTradeDate != "" {
		if list, err := barRepo.GetBarsByDate(preTradeDate); err == nil {
			for i := range list {
				b := &list[i]
				prevBars[b.TsCode] = b
			}
		}
	}

	brk := buildBroker(cfg, db)
	positions, _ := brk.QueryPositions()

	// 模拟盘中每 30 秒刷新一次 (实际生产环境对接实时行情源)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			allBars, err := barRepo.GetBarsByDate(tradeDate)
			if err != nil || len(allBars) == 0 {
				logger.L().Warnf("暂无行情数据, 跳过本次监控")
				continue
			}
			moneyflows := []analysis.MoneyFlow{}
			toplists := []analysis.TopList{}
			snapshot := analysis.MonitorMarket(tradeDate, allBars, prevBars, moneyflows, toplists)

			todayBars := make(map[string]*model.Bar)
			for i := range allBars {
				b := &allBars[i]
				todayBars[b.TsCode] = b
			}
			analysis.SetPrevBars(prevBars)
			snapshot.MonitorPortfolio(positions, todayBars, nil)

			if snapshot.HasDangerAlarm() {
				logger.L().Warnf("[Monitor] 发现危险告警 %d 条", snapshot.AlarmCountByLevel("danger"))
			}
			for _, a := range snapshot.Alarms {
				logger.L().Infof("[Monitor] [%s] %s: %s", a.Level, a.Type, a.Message)
			}
		}
	}
}

// runDiagnose 持仓诊断
func runDiagnose(ctx context.Context, cfg *config.Config, db *sqlx.DB, tradeDate string) error {
	logger.L().Infof("[Diagnose] 持仓诊断 %s", tradeDate)
	stockRepo := store.NewStockRepo(db)
	barRepo := store.NewBarRepo(db)
	stockMap := buildStockMap(stockRepo)

	brk := buildBroker(cfg, db)

	// 用当日行情更新持仓市值(含 ETF)
	allBars, err := barRepo.GetBarsByDate(tradeDate)
	if err == nil && len(allBars) > 0 {
		todayBars := make(map[string]*model.Bar, len(allBars))
		for i := range allBars {
			b := &allBars[i]
			todayBars[b.TsCode] = b
		}
		brk.UpdateMarketValue(todayBars)
	}

	positions, err := brk.QueryPositions()
	if err != nil {
		return fmt.Errorf("查询持仓失败: %w", err)
	}
	asset, err := brk.QueryAsset()
	if err != nil {
		return fmt.Errorf("查询资产失败: %w", err)
	}

	pa := buildPortfolioSummary(positions, asset, stockMap)
	fmt.Println("========== 持仓诊断 ==========")
	fmt.Printf("日期: %s\n", tradeDate)
	fmt.Printf("总资产: ¥%.2f (市值 ¥%.2f + 现金 ¥%.2f)\n", pa.TotalAsset, pa.MarketValue, pa.Cash)
	fmt.Printf("健康度评分: %d/100\n", pa.HealthScore)
	fmt.Printf("集中度: %.1f%%\n", pa.Concentration*100)
	fmt.Println("持仓明细:")
	for _, p := range pa.Positions {
		fmt.Printf("  %s %s | 数量:%d | 成本:%.2f | 现价:%.2f | 盈亏:%.2f (%.2f%%) | 权重:%.1f%% | 建议:%s\n",
			p.TsCode, p.Name, p.TotalQty, p.CostPrice, p.MarketPrice, p.FloatingPnL, p.FloatingPnLPct*100, p.WeightPct*100, p.Advice)
	}
	fmt.Println("==============================")

	// 保存账户快照
	if err := saveSnapshot(db, tradeDate, asset, positions); err != nil {
		logger.L().Warnf("保存账户快照失败: %v", err)
	}
	return nil
}

// runRebalance 调仓建议
func runRebalance(ctx context.Context, cfg *config.Config, db *sqlx.DB, tradeDate string) error {
	logger.L().Infof("[Rebalance] 调仓建议 %s", tradeDate)
	barRepo := store.NewBarRepo(db)
	allBars, err := barRepo.GetBarsByDate(tradeDate)
	if err != nil {
		return fmt.Errorf("获取行情失败: %w", err)
	}
	todayBars := make(map[string]*model.Bar)
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	brk := buildBroker(cfg, db)

	// 用当日行情更新持仓市值(含 ETF)
	brk.UpdateMarketValue(todayBars)

	positions, _ := brk.QueryPositions()
	asset, _ := brk.QueryAsset()

	signals := runStrategy(ctx, cfg, *strategyFlag, todayBars, positions, asset)
	plan := buildRebalanceSummary(signals, positions, asset)

	fmt.Println("========== 调仓建议 ==========")
	fmt.Printf("日期: %s\n", tradeDate)
	fmt.Printf("当前市值: ¥%.2f | 目标市值: ¥%.2f\n", plan.CurrentValue, plan.TargetValue)
	fmt.Printf("调仓理由: %s\n", plan.Reason)
	if len(plan.Orders) == 0 {
		fmt.Println("暂无调仓信号, 建议持仓不动")
	} else {
		fmt.Println("信号列表:")
		for _, s := range plan.Orders {
			dir := "持有"
			if s.Direction == model.DirBuy {
				dir = "买入"
			} else if s.Direction == model.DirSell {
				dir = "卖出"
			}
			fmt.Printf("  [%s] %s | 目标数量:%d | 强度:%.0f%% | %s\n", dir, s.TsCode, s.TargetQty, s.Strength*100, s.Reason)
		}
	}
	fmt.Println("==============================")

	// 保存账户快照
	if err := saveSnapshot(db, tradeDate, asset, positions); err != nil {
		logger.L().Warnf("保存账户快照失败: %v", err)
	}
	return nil
}

// buildBroker 根据配置创建券商实例，并从数据库 seed 持仓
func buildBroker(cfg *config.Config, db *sqlx.DB) broker.Broker {
	switch *brokerFlag {
	case "qmt":
		qmt := broker.NewQMTBridge(cfg.Broker.QMT.URL)
		if err := qmt.Connect(cfg.Broker.QMT.Path, cfg.Broker.QMT.SessionID, cfg.Broker.QMT.AccountID); err != nil {
			logger.L().Warnf("连接 QMT 失败: %v, 回退到 PaperBroker", err)
			return fallbackPaperBroker(cfg, db)
		}
		return qmt
	default:
		return fallbackPaperBroker(cfg, db)
	}
}

func fallbackPaperBroker(cfg *config.Config, db *sqlx.DB) broker.Broker {
	costModel := market.NewCostModel(cfg.Cost)
	pb := broker.NewPaperBroker("paper", cfg.Backtest.InitialCapital, costModel)

	// 从 portfolio 表 seed 持仓到 PaperBroker
	if db != nil {
		seedPaperBrokerFromDB(pb, cfg, db)
	}

	return pb
}

// seedPaperBrokerFromDB 从 portfolio 表读取持仓，注入到 PaperBroker
func seedPaperBrokerFromDB(pb *broker.PaperBroker, cfg *config.Config, db *sqlx.DB) {
	portRepo := store.NewPortfolioRepo(db)
	positions, err := portRepo.GetAllPositions()
	if err != nil || len(positions) == 0 {
		return
	}

	positionMap := make(map[string]*model.Position)
	for _, p := range positions {
		if p.TotalQty <= 0 {
			continue
		}
		positionMap[p.TsCode] = &model.Position{
			TsCode:       p.TsCode,
			TotalQty:     p.TotalQty,
			AvailableQty: p.AvailableQty,
			CostPrice:    p.CostPrice,
		}
	}

	// 优先读取实际 cash，其次 initial_capital，最后 fallback 到 config
	cash := cfg.Backtest.InitialCapital
	if cashStr, err := portRepo.GetMeta("cash"); err == nil && cashStr != "" {
		if v, err := strconv.ParseFloat(cashStr, 64); err == nil && v > 0 {
			cash = v
		}
	} else if capitalStr, err := portRepo.GetMeta("initial_capital"); err == nil && capitalStr != "" {
		if v, err := strconv.ParseFloat(capitalStr, 64); err == nil && v > 0 {
			cash = v
		}
	}

	pb.ImportPositions(positionMap, cash)
	logger.L().Infof("[Seed] 从 portfolio 表恢复 %d 只持仓, 资金 %.2f", len(positionMap), cash)
}

// buildStockMap 构建股票代码->名称映射
func buildStockMap(stockRepo *store.StockRepo) map[string]string {
	m := make(map[string]string)
	stocks, err := stockRepo.GetAll()
	if err != nil {
		return m
	}
	for _, s := range stocks {
		m[s.TsCode] = s.Name
	}
	return m
}

// runStrategy 运行策略产生信号
func runStrategy(ctx context.Context, cfg *config.Config, strategyName string, bars map[string]*model.Bar, positions map[string]*model.Position, asset *broker.AssetInfo) []model.Signal {
	reg := strategy.DefaultRegistry()
	s, ok := reg.Get(strategyName)
	if !ok {
		logger.L().Warnf("策略 %s 未注册, 跳过信号生成", strategyName)
		return nil
	}

	// 构建股票池
	universe := make([]string, 0, len(bars))
	for code := range bars {
		universe = append(universe, code)
	}

	barCtx := &strategy.BarContext{
		TradeDate:  *dateFlag,
		Universe:   universe,
		Bars:       bars,
		Positions:  positions,
		Cash:       asset.Cash,
		TotalAsset: asset.TotalAsset,
		History:    &historyAdapter{}, // 简化历史数据适配器
	}

	if err := s.Init(ctx, nil); err != nil {
		logger.L().Warnf("策略初始化失败: %v", err)
		return nil
	}

	signals, err := s.OnBar(ctx, barCtx)
	if err != nil {
		logger.L().Warnf("策略运行失败: %v", err)
		return nil
	}
	return signals
}

// historyAdapter 简化历史数据适配器 (占位)
type historyAdapter struct{}

func (h *historyAdapter) GetBars(tsCode, endDate string, n int) ([]model.Bar, error) {
	return nil, nil
}

func (h *historyAdapter) GetCloses(tsCode, endDate string, n int) ([]float64, error) {
	return nil, nil
}

// buildPortfolioSummary 构建持仓分析展示数据
func buildPortfolioSummary(positions map[string]*model.Position, asset *broker.AssetInfo, stockMap map[string]string) *report.PortfolioSummary {
	pa := &report.PortfolioSummary{
		TradeDate:     *dateFlag,
		TotalAsset:    asset.TotalAsset,
		MarketValue:   asset.MarketValue,
		Cash:          asset.Cash,
		SectorDist:    make(map[string]float64),
		HealthScore:   80, // 默认评分
	}

	if pa.TotalAsset <= 0 {
		pa.TotalAsset = asset.Cash + asset.MarketValue
	}

	var maxWeight float64
	for tsCode, pos := range positions {
		if pos.TotalQty <= 0 {
			continue
		}
		weight := 0.0
		if pa.TotalAsset > 0 {
			weight = pos.MarketValue / pa.TotalAsset
		}
		if weight > maxWeight {
			maxWeight = weight
		}

		// 简单 sector 推断
		sector := model.MarketFromCode(tsCode)
		pa.SectorDist[sector] += weight

		p := report.PositionSummary{
			TsCode:         tsCode,
			Name:           stockMap[tsCode],
			TotalQty:       pos.TotalQty,
			CostPrice:      pos.CostPrice,
			MarketPrice:    pos.MarketPrice,
			MarketValue:    pos.MarketValue,
			FloatingPnL:    pos.FloatingPnL,
			FloatingPnLPct: pos.FloatingPnLPct,
			WeightPct:      weight,
			Sector:         sector,
			Advice:         adviceFromPnL(pos.FloatingPnLPct),
			RiskLevel:      riskFromPnL(pos.FloatingPnLPct),
		}
		pa.Positions = append(pa.Positions, p)
	}
	pa.Concentration = maxWeight

	// 根据集中度调整健康度评分
	if pa.Concentration > 0.5 {
		pa.HealthScore -= 20
	}
	if pa.HealthScore < 0 {
		pa.HealthScore = 0
	}
	if pa.HealthScore > 100 {
		pa.HealthScore = 100
	}

	return pa
}

func adviceFromPnL(pct float64) string {
	if pct < -0.1 {
		return "sell"
	}
	if pct > 0.2 {
		return "sell" // 止盈
	}
	if pct < 0 {
		return "hold"
	}
	return "hold"
}

func riskFromPnL(pct float64) string {
	if pct < -0.1 {
		return "high"
	}
	if pct < -0.05 {
		return "medium"
	}
	return "low"
}

// buildRebalanceSummary 构建调仓计划展示数据
func buildRebalanceSummary(signals []model.Signal, positions map[string]*model.Position, asset *broker.AssetInfo) *report.RebalanceSummary {
	plan := &report.RebalanceSummary{
		TradeDate:    *dateFlag,
		CurrentValue: asset.MarketValue,
		TargetValue:  asset.TotalAsset,
		Reason:       "基于策略信号生成",
	}

	for _, s := range signals {
		plan.Orders = append(plan.Orders, report.SignalSummary{
			TsCode:    s.TsCode,
			Direction: s.Direction,
			TargetQty: s.TargetQty,
			Reason:    s.Reason,
			Strength:  s.Strength,
		})
	}

	if len(signals) == 0 {
		plan.Reason = "无明确调仓信号, 建议维持当前持仓"
	} else {
		buyCount, sellCount := 0, 0
		for _, s := range signals {
			if s.Direction == model.DirBuy {
				buyCount++
			} else if s.Direction == model.DirSell {
				sellCount++
			}
		}
		plan.Reason = fmt.Sprintf("策略产生 %d 个买入信号, %d 个卖出信号", buyCount, sellCount)
	}
	return plan
}

// buildStrategySummary 构建策略建议展示数据
func buildStrategySummary(strategyName string, signals []model.Signal, pa *report.PortfolioSummary) *report.StrategySummary {
	action := "hold"
	confidence := 0.5
	reason := "当前市场处于震荡区间, 建议观望"
	riskNote := "注意控制仓位, 避免追涨杀跌"

	if len(signals) > 0 {
		buyCount, sellCount := 0, 0
		for _, s := range signals {
			if s.Direction == model.DirBuy {
				buyCount++
			} else if s.Direction == model.DirSell {
				sellCount++
			}
		}
		if buyCount > sellCount {
			action = "buy"
			confidence = 0.6 + float64(buyCount-sellCount)*0.05
			reason = fmt.Sprintf("策略偏向做多, 发现 %d 个买入机会", buyCount)
		} else if sellCount > buyCount {
			action = "sell"
			confidence = 0.6 + float64(sellCount-buyCount)*0.05
			reason = fmt.Sprintf("策略偏向风控, 发现 %d 个卖出信号", sellCount)
		} else {
			action = "rebalance"
			confidence = 0.55
			reason = "多空信号均衡, 建议适度调仓优化结构"
		}
	}

	if pa != nil && pa.HealthScore < 50 {
		riskNote = "持仓健康度较低, 建议优先处理风险持仓"
	}

	if confidence > 1.0 {
		confidence = 1.0
	}

	return &report.StrategySummary{
		StrategyName:      strategyName,
		Confidence:        confidence,
		Reason:            reason,
		RecommendedAction: action,
		RiskNote:          riskNote,
	}
}

// buildActionItems 构建操作清单
func buildActionItems(signals []model.Signal, pa *report.PortfolioSummary, ms *analysis.MarketSnapshot) []report.ActionItem {
	var items []report.ActionItem

	// 开盘前: 检查新闻和大盘
	items = append(items, report.ActionItem{
		Time:     "09:25",
		Action:   "检查",
		Detail:   "查看隔夜新闻、外围市场、集合竞价情况",
		Priority: 1,
	})

	// 开盘后: 卖出优先
	for _, s := range signals {
		if s.Direction == model.DirSell {
			items = append(items, report.ActionItem{
				Time:     "09:30",
				Action:   "卖出",
				TsCode:   s.TsCode,
				Detail:   s.Reason,
				Priority: 1,
			})
		}
	}

	// 盘中: 监控告警
	if ms != nil && len(ms.Alarms) > 0 {
		for _, a := range ms.Alarms {
			items = append(items, report.ActionItem{
				Time:     "盘中",
				Action:   "检查",
				TsCode:   a.TsCode,
				Detail:   a.Message,
				Priority: alarmPriority(a.Level),
			})
		}
	}

	// 盘中: 买入
	for _, s := range signals {
		if s.Direction == model.DirBuy {
			items = append(items, report.ActionItem{
				Time:     "盘中",
				Action:   "买入",
				TsCode:   s.TsCode,
				Detail:   s.Reason,
				Priority: 3,
			})
		}
	}

	// 尾盘
	items = append(items, report.ActionItem{
		Time:     "14:50",
		Action:   "检查",
		Detail:   "检查未完成订单, 决定是否留仓过夜",
		Priority: 2,
	})

	// 盘后
	items = append(items, report.ActionItem{
		Time:     "盘后",
		Action:   "检查",
		Detail:   "复盘当日操作, 更新交易日志",
		Priority: 3,
	})

	return items
}

func alarmPriority(level string) int {
	switch level {
	case "danger":
		return 1
	case "warning":
		return 2
	default:
		return 3
	}
}

// extractBuyTargets 从信号中提取买入目标
func extractBuyTargets(signals []model.Signal) []string {
	var targets []string
	for _, s := range signals {
		if s.Direction == model.DirBuy {
			targets = append(targets, s.TsCode)
		}
	}
	return targets
}

// extractTsCodes 从持仓中提取代码列表
func extractTsCodes(positions map[string]*model.Position) []string {
	var codes []string
	for code := range positions {
		codes = append(codes, code)
	}
	return codes
}
