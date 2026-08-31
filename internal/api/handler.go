package api

import (
	"fmt"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/agent"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/internal/tushare"
	"jingzhe-trader/pkg/logger"
)

// ==================== JSON 响应结构 ====================

// DailyReportJSON 每日操盘报告 (JSON格式)
type DailyReportJSON struct {
	Date           string              `json:"date"`
	MarketSnapshot *MarketSnapshotJSON `json:"market_snapshot"`
	Portfolio      *PortfolioJSON      `json:"portfolio"`
	Rebalance      *RebalanceJSON      `json:"rebalance"`
	StrategyAdvice *StrategyJSON       `json:"strategy_advice"`
	News           *NewsJSON           `json:"news"`
	ActionItems    []ActionItemJSON    `json:"action_items"`
}

// MarketSnapshotJSON 市场快照
type MarketSnapshotJSON struct {
	UpCount        int                      `json:"up_count"`
	DownCount      int                      `json:"down_count"`
	LimitUpCount   int                      `json:"limit_up_count"`
	LimitDownCount int                      `json:"limit_down_count"`
	VolumeRatio    float64                  `json:"volume_ratio"`
	HotSectors     []map[string]interface{} `json:"hot_sectors"`
	Alarms         []map[string]string      `json:"alarms"`
}

// PortfolioJSON 持仓诊断
type PortfolioJSON struct {
	TotalAsset    float64                  `json:"total_asset"`
	Cash          float64                  `json:"cash"`
	MarketValue   float64                  `json:"market_value"`
	DailyPnLPct   float64                  `json:"daily_pnl_pct"`
	HealthScore   float64                  `json:"health_score"`
	Concentration map[string]float64       `json:"concentration"`
	SectorDist    []map[string]interface{} `json:"sector_distribution"`
	PnLSummary    map[string]interface{}   `json:"pnl_summary"`
	RiskMetrics   map[string]interface{}   `json:"risk_metrics"`
	Holdings      []map[string]interface{} `json:"holdings"`
}

// RebalanceJSON 调仓计划
type RebalanceJSON struct {
	SellList []TradeSuggestionJSON `json:"sell_list"`
	BuyList  []TradeSuggestionJSON `json:"buy_list"`
	HoldList []HoldSuggestionJSON  `json:"hold_list"`
	CashPct  float64               `json:"cash_pct"`
	Reason   string                `json:"reason"`
}

// TradeSuggestionJSON 交易建议
type TradeSuggestionJSON struct {
	TsCode   string  `json:"ts_code"`
	Name     string  `json:"name"`
	Action   string  `json:"action"`
	DeltaQty int     `json:"delta_qty"`
	Price    float64 `json:"price"`
	Amount   float64 `json:"amount"`
	Priority int     `json:"priority"`
	Reason   string  `json:"reason"`
	Urgency  string  `json:"urgency"`
}

// HoldSuggestionJSON 持有建议
type HoldSuggestionJSON struct {
	TsCode      string  `json:"ts_code"`
	Name        string  `json:"name"`
	Qty         int     `json:"qty"`
	CostPrice   float64 `json:"cost_price"`
	MarketPrice float64 `json:"market_price"`
	FloatingPnL float64 `json:"floating_pnl"`
	Suggestion  string  `json:"suggestion"`
}

// StrategyJSON 策略建议
type StrategyJSON struct {
	Recommended string  `json:"recommended"`
	Confidence  float64 `json:"confidence"`
	Reason      string  `json:"reason"`
	Condition   string  `json:"condition"`
}

// NewsJSON 新闻摘要
type NewsJSON struct {
	Sentiment   string              `json:"sentiment"`
	RelatedNews []map[string]string `json:"related_news"`
}

// ActionItemJSON 操作项
type ActionItemJSON struct {
	Time     string `json:"time"`
	Action   string `json:"action"`
	TsCode   string `json:"ts_code"`
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	Priority int    `json:"priority"`
}

// ==================== Service ====================

// Service API 服务
type Service struct {
	cfg                *config.Config
	db                 *sqlx.DB
	barRepo            *store.BarRepo
	calRepo            *store.CalendarRepo
	basicRepo          *store.BasicRepo
	finaRepo           *store.FinaRepo
	newsRepo           *store.NewsRepo
	debateRepo         *store.DebateRepo
	alertRepo          *store.AlertRepo
	planRepo           *store.PlanRepo
	actionRepo         *store.ActionRepo
	jobRepo            *store.JobRepo
	stockRepo          *store.StockRepo
	screenRepo         *store.ScreenRepo  // 选股结果仓库
	screener           *screener.Screener // 自动选股器
	stockMap           map[string]string  // ts_code -> name
	stockMapMu         sync.RWMutex       // 保护 stockMap 并发读写
	brk                broker.Broker
	dynamicSelector    *strategy.DynamicSelector    // 动态策略选择器
	strategyCache      map[string]strategy.Strategy // 策略实例缓存 (避免每次重建丢失状态)
	strategyCacheMu    sync.RWMutex                 // 保护策略缓存并发读写
	llmClient          *llm.Client                  // LLM 客户端
	debateOrchestrator *agent.DebateOrchestrator    // 智能体辩论编排器
	startTime          time.Time                    // 服务启动时间 (uptime用)
	updateMu           sync.Mutex                   // 数据更新互斥 (防并发重入)
	goalTracker        *goal.Tracker                // 季度目标跟踪器 (nil=未启用)
}

// NewService 创建 API 服务
// db 由组合根 (cmd) 打开并注入: 配置本身就存在这个库里, 组件内再按 cfg 路径开一次库
// 会出现 "-db 指测试库、服务却连生产库" 的错位, 故连接所有权始终留在 cmd
func NewService(db *sqlx.DB, cfg *config.Config) (*Service, error) {
	if db == nil {
		return nil, fmt.Errorf("缺少数据库连接")
	}

	svc := &Service{
		cfg:           cfg,
		db:            db,
		barRepo:       store.NewBarRepo(db),
		calRepo:       store.NewCalendarRepo(db),
		basicRepo:     store.NewBasicRepo(db),
		finaRepo:      store.NewFinaRepo(db),
		newsRepo:      store.NewNewsRepo(db),
		debateRepo:    store.NewDebateRepo(db),
		alertRepo:     store.NewAlertRepo(db),
		planRepo:      store.NewPlanRepo(db),
		actionRepo:    store.NewActionRepo(db),
		jobRepo:       store.NewJobRepo(db),
		stockRepo:     store.NewStockRepo(db),
		screenRepo:    store.NewScreenRepo(db),
		strategyCache: make(map[string]strategy.Strategy),
		startTime:     time.Now(),
	}
	if cfg.Goal.Enabled {
		svc.goalTracker = goal.NewTracker(cfg.Goal, store.NewTradeRepo(db), store.NewPortfolioRepo(db), liveSnapshotRunID)
	}

	// 加载股票名称映射
	svc.loadStockMap()

	// 初始化券商: QMT 实盘模式用长生命周期 QMTBridge (成交回报轮询依赖其上注册的回调),
	// 其余模式用 paper 模拟盘。此前硬编码 PaperBroker 导致 QMT 模式下 PollBrokerTrades
	// 的类型断言永远失败, 真实成交无法落库 (2026-08 审计 P0)
	costModel := market.NewCostModel(cfg.Cost)
	if cfg.Broker.Type == "qmt" && cfg.Broker.QMT.URL != "" {
		svc.brk = broker.NewQMTBridge(cfg.Broker.QMT.URL)
	} else {
		svc.brk = broker.NewPaperBroker("api", cfg.Backtest.InitialCapital, costModel)
	}

	// 成交回调 → action_log(kind=trade) 落库 (broker 侧成交/人工确认之外实盘成交的持久化入口)
	// QMT 模式下由调度器对账任务 PollTrades 触发; paper 模式下由 RecordTrade/auto-execute 触发
	svc.brk.OnTrade(func(trade model.Trade) {
		if trade.TsCode == "" || trade.Qty <= 0 {
			return
		}
		if err := svc.actionRepo.AddTrade(trade.TradeDate, store.TradeFill{
			TsCode: trade.TsCode,
			Side:   trade.Side.String(),
			Qty:    trade.Qty,
			Price:  trade.Price,
			Amount: trade.Amount,
		}); err != nil {
			logger.L().Warnw("成交回调落库失败", "ts_code", trade.TsCode, "err", err)
		}
	})

	// 初始化扩展功能（动态策略选择器、策略缓存、持仓恢复）
	svc.initExtensions()

	// 初始化 LLM 客户端和新闻分析器
	llmCfg := llm.Config{
		APIKey:         cfg.LLM.APIKey,
		BaseURL:        cfg.LLM.BaseURL,
		Model:          cfg.LLM.Model,
		Enabled:        cfg.LLM.Enabled,
		Temperature:    cfg.LLM.Temperature,
		MaxTokens:      cfg.LLM.MaxTokens,
		TimeoutSeconds: cfg.LLM.TimeoutSeconds,
		JSONMode:       &cfg.LLM.JSONMode,
		MaxConcurrency: cfg.LLM.MaxConcurrency,
		RPS:            cfg.LLM.RPS,
	}
	svc.llmClient = llm.NewClient(llmCfg)

	// 初始化智能体辩论编排器 (复用 Service 的共享 Repo, 避免重复实例化)
	// 风控约束与指数清单由配置注入: 此前提示词把仓位上限写死成 60%, 而 risk.max_position_pct=0.40
	svc.debateOrchestrator = agent.NewDebateOrchestrator(
		svc.llmClient,
		svc.barRepo,
		svc.basicRepo,
		svc.finaRepo,
		svc.newsRepo,
		svc.debateRepo,
		store.NewDebateReviewRepo(db),
		store.NewMoneyFlowRepo(db),
		store.NewTopListRepo(db),
		agent.DebateSettings{
			Positions: agent.PositionLimits{
				MaxPositionPct:      cfg.Risk.MaxPositionPct,
				MaxTotalPositionPct: cfg.Risk.MaxTotalPositionPct,
				StopLossPct:         cfg.Risk.StopLossPct,
			},
			MarketIndexes: watchlistIndexCodes(cfg.Dataloader.Watchlist),
		},
	)

	// 初始化自动选股器 (全市场筛选, 补充配置股票池)
	tsClient := tushare.NewClient(cfg.Tushare)
	screenerCfg := cfg.Screener
	// 不再排除配置池, universe 可以为空 (全部依赖自动选股)
	svc.screener = screener.New(tsClient, svc.stockRepo, svc.barRepo, svc.screenRepo, screenerCfg)

	return svc, nil
}

// barsToMap 将日线切片转为按代码索引的 map (8 处调用点共用, 消除重复转换样板)
func barsToMap(bars []model.Bar) map[string]*model.Bar {
	m := make(map[string]*model.Bar, len(bars))
	for i := range bars {
		b := &bars[i]
		m[b.TsCode] = b
	}
	return m
}

// Broker 返回组合根注入的长生命周期 broker 实例
// 调度器等外部组件应复用此实例, 禁止自行 NewQMTBridge (新建实例丢失 OnTrade 成交回调)
func (s *Service) Broker() broker.Broker {
	return s.brk
}

// loadStockMap 加载股票名称映射 (线程安全)
func (s *Service) loadStockMap() {
	stockRepo := store.NewStockRepo(s.db)
	stocks, err := stockRepo.GetAll()
	m := make(map[string]string)
	if err == nil {
		for _, st := range stocks {
			m[st.TsCode] = st.Name
		}
	}
	s.stockMapMu.Lock()
	s.stockMap = m
	s.stockMapMu.Unlock()
}

// stockName 获取股票名称 (线程安全读)
func (s *Service) stockName(tsCode string) string {
	s.stockMapMu.RLock()
	defer s.stockMapMu.RUnlock()
	if name, ok := s.stockMap[tsCode]; ok {
		return name
	}
	return tsCode
}

// DebateOrchestrator 返回智能体辩论编排器 (供调度器调用决策变更检测)
func (s *Service) DebateOrchestrator() *agent.DebateOrchestrator {
	return s.debateOrchestrator
}

// Screener 返回选股器实例 (供调度器调用)
func (s *Service) Screener() *screener.Screener {
	return s.screener
}

// ScreenRepo 返回选股结果仓库
func (s *Service) ScreenRepo() *store.ScreenRepo {
	return s.screenRepo
}

// DB 返回底层数据库连接 (供调度器等外部组件复用同一连接)
// 连接由组合根打开并持有, 关闭责任也在组合根, Service 不得自行 Close
func (s *Service) DB() *sqlx.DB {
	return s.db
}

// ==================== 共享内部方法 ====================

// getPrevBars 获取上一交易日行情
func (s *Service) getPrevBars(date string) map[string]*model.Bar {
	preTradeDate, err := s.calRepo.GetPreTradeDate(date)
	if err != nil || preTradeDate == "" {
		return map[string]*model.Bar{}
	}
	prevBarsList, err := s.barRepo.GetBarsByDate(preTradeDate)
	if err != nil {
		return map[string]*model.Bar{}
	}
	return barsToMap(prevBarsList)
}

// getPositions 获取持仓 (出错返回空 map)
func (s *Service) getPositions() map[string]*model.Position {
	positions, err := s.brk.QueryPositions()
	if err != nil {
		return make(map[string]*model.Position)
	}
	if positions == nil {
		return make(map[string]*model.Position)
	}
	return positions
}

// getAsset 获取资产信息 (出错返回默认)
func (s *Service) getAsset() *broker.AssetInfo {
	asset, err := s.brk.QueryAsset()
	if err != nil {
		return &broker.AssetInfo{Cash: s.cfg.Backtest.InitialCapital}
	}
	return asset
}

// dbHistoryAdapter 基于数据库的历史数据适配器 (策略均线计算/持仓分析 Beta/VaR 用)
type dbHistoryAdapter struct {
	barRepo *store.BarRepo
}

func (h *dbHistoryAdapter) GetBars(tsCode, endDate string, n int) ([]model.Bar, error) {
	// 自然日回退 n*2+10 天覆盖 n 个交易日
	start := endDate
	if t, err := time.Parse("20060102", endDate); err == nil {
		start = t.AddDate(0, 0, -(n*2 + 10)).Format("20060102")
	}
	bars, err := h.barRepo.GetBars(tsCode, start, endDate)
	if err != nil {
		return nil, fmt.Errorf("查询历史行情失败 %s: %w", tsCode, err)
	}
	if len(bars) > n {
		bars = bars[len(bars)-n:]
	}
	// 前复权, 与回测 DataProvider 保持同一口径, 避免除权日产生假信号
	model.AdjustBarsForward(bars)
	return bars, nil
}

func (h *dbHistoryAdapter) GetCloses(tsCode, endDate string, n int) ([]float64, error) {
	bars, err := h.GetBars(tsCode, endDate, n)
	if err != nil {
		return nil, fmt.Errorf("查询历史收盘失败 %s: %w", tsCode, err)
	}
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	return closes, nil
}

// scoreToLabel 将情感分数转换为中文标签
func scoreToLabel(score float64) string {
	if score > 0.3 {
		return "积极"
	} else if score < -0.3 {
		return "消极"
	}
	return "中性"
}

// formatDate 格式化日期字符串 20260715 -> 2026-07-15
func formatDate(dateStr string) string {
	if len(dateStr) != 8 {
		return dateStr
	}
	return dateStr[:4] + "-" + dateStr[4:6] + "-" + dateStr[6:8]
}
