package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/maintenance"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"

	"jingzhe-trader/web"
)

// ==================== JSON 响应结构 ====================

// APIResponse 统一 API 响应格式
type APIResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data interface{}     `json:"data"`
}

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
	TotalAsset    float64                      `json:"total_asset"`
	Cash          float64                      `json:"cash"`
	MarketValue   float64                      `json:"market_value"`
	DailyPnLPct   float64                      `json:"daily_pnl_pct"`
	HealthScore   float64                      `json:"health_score"`
	Concentration map[string]float64           `json:"concentration"`
	SectorDist    []map[string]interface{}     `json:"sector_distribution"`
	PnLSummary    map[string]interface{}       `json:"pnl_summary"`
	RiskMetrics   map[string]interface{}       `json:"risk_metrics"`
	Holdings      []map[string]interface{}     `json:"holdings"`
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
	Sentiment   string                `json:"sentiment"`
	RelatedNews []map[string]string   `json:"related_news"`
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
	cfg             *config.Config
	db              *sqlx.DB
	barRepo         *store.BarRepo
	calRepo         *store.CalendarRepo
	stockMap        map[string]string // ts_code -> name
	brk             broker.Broker
	dynamicSelector *strategy.DynamicSelector // 动态策略选择器
	updater         *maintenance.AutoUpdater  // 自动维护器
	llmClient       *llm.Client                // LLM 客户端
	llmNews         *llm.NewsAnalyzer          // LLM 新闻分析器
}

// NewService 创建 API 服务
func NewService(cfg *config.Config) (*Service, error) {
	db, err := store.NewDB(cfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("初始化数据库失败: %w", err)
	}

	svc := &Service{
		cfg:     cfg,
		db:      db,
		barRepo: store.NewBarRepo(db),
		calRepo: store.NewCalendarRepo(db),
	}

	// 加载股票名称映射
	svc.loadStockMap()

	// 初始化券商 (使用 paper broker)
	costModel := market.NewCostModel(cfg.Cost)
	svc.brk = broker.NewPaperBroker("api", cfg.Backtest.InitialCapital, costModel)

	// 初始化扩展功能（动态策略选择器、自动维护器、持仓恢复）
	svc.initExtensions()

	// 初始化 LLM 客户端和新闻分析器
	llmCfg := llm.Config{
		APIKey:  cfg.LLM.APIKey,
		BaseURL: cfg.LLM.BaseURL,
		Model:   cfg.LLM.Model,
		Enabled: cfg.LLM.Enabled,
	}
	svc.llmClient = llm.NewClient(llmCfg)
	svc.llmNews = llm.NewNewsAnalyzer(svc.llmClient)

	return svc, nil
}

// loadStockMap 加载股票名称映射
func (s *Service) loadStockMap() {
	stockRepo := store.NewStockRepo(s.db)
	stocks, err := stockRepo.GetAll()
	if err != nil {
		s.stockMap = make(map[string]string)
		return
	}
	s.stockMap = make(map[string]string, len(stocks))
	for _, st := range stocks {
		s.stockMap[st.TsCode] = st.Name
	}
}

// stockName 获取股票名称
func (s *Service) stockName(tsCode string) string {
	if name, ok := s.stockMap[tsCode]; ok {
		return name
	}
	return tsCode
}

// Close 释放资源
func (s *Service) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// ==================== 核心业务方法 ====================

// RunDaily 生成每日操盘报告 (JSON)
func (s *Service) RunDaily(date string, strategyName string) (*DailyReportJSON, error) {
	// 1. 获取当日全市场行情
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取当日行情失败: %w", err)
	}
	if len(allBars) == 0 {
		return nil, fmt.Errorf("当日 %s 无行情数据", date)
	}

	// 2. 构建当日行情 map
	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	// 3. 获取上一交易日行情
	prevBars := s.getPrevBars(date)
	analysis.SetPrevBars(prevBars)

	// 4. 获取持仓和资产
	positions := s.getPositions()
	asset := s.getAsset()
	s.brk.UpdateMarketValue(todayBars)
	positions, _ = s.brk.QueryPositions()
	asset, _ = s.brk.QueryAsset()

	// 5. 市场快照
	marketSnapshot := s.buildMarketSnapshot(date, allBars, prevBars)

	// 6. 策略信号
	signals := s.runStrategy(date, strategyName, todayBars, positions, asset)

	// 7. 持仓诊断
	portfolioJSON := s.buildPortfolioJSON(positions, asset, todayBars)

	// 8. 调仓计划
	rebalanceJSON := s.buildRebalanceJSON(date, signals, positions, asset, todayBars)

	// 9. 策略建议
	strategyJSON := s.buildStrategyJSON(date, signals, todayBars, portfolioJSON)

	// 10. 新闻摘要
	newsJSON := s.buildNewsJSON()

	// 11. 操作清单
	actionItems := s.buildActionItems(signals, portfolioJSON, marketSnapshot)

	return &DailyReportJSON{
		Date:           date,
		MarketSnapshot: marketSnapshot,
		Portfolio:      portfolioJSON,
		Rebalance:      rebalanceJSON,
		StrategyAdvice: strategyJSON,
		News:           newsJSON,
		ActionItems:    actionItems,
	}, nil
}

// RunPositions 持仓诊断
func (s *Service) RunPositions(date string) (*PortfolioJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	positions := s.getPositions()
	asset := s.getAsset()
	s.brk.UpdateMarketValue(todayBars)
	positions, _ = s.brk.QueryPositions()
	asset, _ = s.brk.QueryAsset()

	return s.buildPortfolioJSON(positions, asset, todayBars), nil
}

// RunRebalance 调仓建议
func (s *Service) RunRebalance(date string, strategyName string) (*RebalanceJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	positions := s.getPositions()
	asset := s.getAsset()
	s.brk.UpdateMarketValue(todayBars)
	positions, _ = s.brk.QueryPositions()
	asset, _ = s.brk.QueryAsset()

	signals := s.runStrategy(date, strategyName, todayBars, positions, asset)
	return s.buildRebalanceJSON(date, signals, positions, asset, todayBars), nil
}

// RunNews 新闻舆情
func (s *Service) RunNews() (*NewsJSON, error) {
	return s.buildNewsJSON(), nil
}

// RunStrategy 策略建议
func (s *Service) RunStrategy(date string, strategyName string) (*StrategyJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	positions := s.getPositions()
	asset := s.getAsset()

	signals := s.runStrategy(date, strategyName, todayBars, positions, asset)
	return s.buildStrategyJSON(date, signals, todayBars, nil), nil
}

// RunMarket 市场概况
func (s *Service) RunMarket(date string) (*MarketSnapshotJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}
	if len(allBars) == 0 {
		return nil, fmt.Errorf("当日 %s 无行情数据", date)
	}

	prevBars := s.getPrevBars(date)
	return s.buildMarketSnapshot(date, allBars, prevBars), nil
}

// ==================== 内部构建方法 ====================

// getPrevBars 获取上一交易日行情
func (s *Service) getPrevBars(date string) map[string]*model.Bar {
	prevBars := make(map[string]*model.Bar)
	preTradeDate, err := s.calRepo.GetPreTradeDate(date)
	if err != nil || preTradeDate == "" {
		return prevBars
	}
	prevBarsList, err := s.barRepo.GetBarsByDate(preTradeDate)
	if err != nil {
		return prevBars
	}
	for i := range prevBarsList {
		b := &prevBarsList[i]
		prevBars[b.TsCode] = b
	}
	return prevBars
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

// buildMarketSnapshot 构建市场快照 JSON
func (s *Service) buildMarketSnapshot(date string, allBars []model.Bar, prevBars map[string]*model.Bar) *MarketSnapshotJSON {
	moneyflows := []analysis.MoneyFlow{}
	toplists := []analysis.TopList{}
	snapshot := analysis.MonitorMarket(date, allBars, prevBars, moneyflows, toplists)

	result := &MarketSnapshotJSON{
		UpCount:        snapshot.UpCount,
		DownCount:      snapshot.DownCount,
		LimitUpCount:   snapshot.UpLimitCount,
		LimitDownCount: snapshot.DownLimitCount,
		VolumeRatio:    snapshot.VolumeRatio,
		HotSectors:     make([]map[string]interface{}, 0, len(snapshot.HotSectors)),
		Alarms:         make([]map[string]string, 0, len(snapshot.Alarms)),
	}

	for _, hs := range snapshot.HotSectors {
		result.HotSectors = append(result.HotSectors, map[string]interface{}{
			"sector":        hs.Sector,
			"avg_change":    hs.AvgChange,
			"leader_stock":  hs.LeaderStock,
			"leader_change": hs.LeaderChange,
		})
	}

	for _, a := range snapshot.Alarms {
		result.Alarms = append(result.Alarms, map[string]string{
			"level":   a.Level,
			"type":    a.Type,
			"ts_code": a.TsCode,
			"message": a.Message,
		})
	}

	return result
}

// buildPortfolioJSON 构建持仓诊断 JSON
func (s *Service) buildPortfolioJSON(
	positions map[string]*model.Position,
	asset *broker.AssetInfo,
	todayBars map[string]*model.Bar,
) *PortfolioJSON {
	totalAsset := asset.TotalAsset
	cash := asset.Cash
	marketValue := asset.MarketValue

	if totalAsset <= 0 {
		totalAsset = cash + marketValue
	}

	result := &PortfolioJSON{
		TotalAsset:  totalAsset,
		Cash:        cash,
		MarketValue: marketValue,
		HealthScore: 80,
		Concentration: map[string]float64{
			"top1_pct": 0,
			"top3_pct": 0,
			"top5_pct": 0,
		},
		SectorDist: []map[string]interface{}{},
		PnLSummary: map[string]interface{}{
			"total_pnl": 0.0,
			"win_count": 0,
			"loss_count": 0,
			"win_pct":   0.0,
		},
		RiskMetrics: map[string]interface{}{
			"max_loss_pct": 0.0,
			"var95":        0.0,
		},
		Holdings: []map[string]interface{}{},
	}

	if totalAsset <= 0 {
		return result
	}

	// 按市值排序的持仓列表
	type posInfo struct {
		tsCode      string
		pos         *model.Position
		marketValue float64
		weight      float64
	}
	var posList []posInfo
	var totalPnL float64
	var winCount, lossCount int
	var maxLossPct float64

	sectorMap := make(map[string]float64)

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		mv := pos.MarketValue
		if mv <= 0 && pos.MarketPrice > 0 {
			mv = float64(pos.TotalQty) * pos.MarketPrice
		}
		if mv <= 0 {
			continue
		}

		weight := mv / totalAsset
		posList = append(posList, posInfo{
			tsCode:      tsCode,
			pos:         pos,
			marketValue: mv,
			weight:      weight,
		})

		totalPnL += pos.FloatingPnL
		if pos.FloatingPnL > 0 {
			winCount++
		} else if pos.FloatingPnL < 0 {
			lossCount++
		}
		if pos.FloatingPnLPct < maxLossPct {
			maxLossPct = pos.FloatingPnLPct
		}

		// 板块分布
		sector := model.MarketFromCode(tsCode)
		sectorMap[sector] += weight

		// 持仓明细
		result.Holdings = append(result.Holdings, map[string]interface{}{
			"ts_code":      tsCode,
			"name":         s.stockName(tsCode),
			"total_qty":    pos.TotalQty,
			"cost_price":   pos.CostPrice,
			"market_price": pos.MarketPrice,
			"market_value": mv,
			"floating_pnl": pos.FloatingPnL,
			"pnl_pct":      pos.FloatingPnLPct,
			"weight_pct":   weight,
			"sector":       sector,
		})
	}

	// 按市值降序排序
	for i := 0; i < len(posList)-1; i++ {
		for j := i + 1; j < len(posList); j++ {
			if posList[j].marketValue > posList[i].marketValue {
				posList[i], posList[j] = posList[j], posList[i]
			}
		}
	}

	// 集中度
	if len(posList) >= 1 {
		result.Concentration["top1_pct"] = posList[0].weight
	}
	if len(posList) >= 3 {
		var sum float64
		for i := 0; i < 3; i++ {
			sum += posList[i].weight
		}
		result.Concentration["top3_pct"] = sum
	}
	if len(posList) >= 5 {
		var sum float64
		for i := 0; i < 5; i++ {
			sum += posList[i].weight
		}
		result.Concentration["top5_pct"] = sum
	}

	// 板块分布
	for sector, weight := range sectorMap {
		result.SectorDist = append(result.SectorDist, map[string]interface{}{
			"sector": sector,
			"weight": weight,
		})
	}

	// 盈亏摘要
	totalCount := winCount + lossCount
	winPct := 0.0
	if totalCount > 0 {
		winPct = float64(winCount) / float64(totalCount)
	}
	result.PnLSummary = map[string]interface{}{
		"total_pnl":  totalPnL,
		"win_count":  winCount,
		"loss_count": lossCount,
		"win_pct":    winPct,
	}

	// 风险指标
	result.RiskMetrics = map[string]interface{}{
		"max_loss_pct": maxLossPct,
		"var95":        0.0,
	}

	// 健康度评分 (简化版)
	healthScore := 80.0
	if top1Pct, ok := result.Concentration["top1_pct"]; ok && top1Pct > 0.5 {
		healthScore -= 20
	}
	if winPct < 0.5 && len(posList) > 0 {
		healthScore -= 10
	}
	if maxLossPct < -0.1 {
		healthScore -= 15
	}
	if healthScore < 0 {
		healthScore = 0
	}
	if healthScore > 100 {
		healthScore = 100
	}
	result.HealthScore = healthScore

	// 计算日收益率（对比昨日快照）
	var prevTotalAsset float64
	err := s.db.Get(&prevTotalAsset, "SELECT total_asset FROM account_snapshot ORDER BY trade_date DESC LIMIT 1")
	if err == nil && prevTotalAsset > 0 {
		result.DailyPnLPct = (totalAsset - prevTotalAsset) / prevTotalAsset
	}

	return result
}

// buildRebalanceJSON 构建调仓计划 JSON
func (s *Service) buildRebalanceJSON(
	date string,
	signals []model.Signal,
	positions map[string]*model.Position,
	asset *broker.AssetInfo,
	todayBars map[string]*model.Bar,
) *RebalanceJSON {
	totalAsset := asset.TotalAsset
	if totalAsset <= 0 {
		totalAsset = asset.Cash + asset.MarketValue
	}

	// 构建信号 map
	signalMap := make(map[string]model.Signal)
	for _, sig := range signals {
		signalMap[sig.TsCode] = sig
	}

	// 遍历持仓, 分类卖出/持有
	var sellList []TradeSuggestionJSON
	var holdList []HoldSuggestionJSON

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		price := pos.MarketPrice
		if bar, ok := todayBars[tsCode]; ok && bar.Close > 0 {
			price = bar.Close
		}
		if price <= 0 {
			continue
		}

		sig, hasSignal := signalMap[tsCode]

		// 止损检查
		if pos.FloatingPnLPct < -0.05 {
			sellQty := pos.AvailableQty
			if sellQty > 0 {
				sellList = append(sellList, TradeSuggestionJSON{
					TsCode:   tsCode,
					Name:     s.stockName(tsCode),
					Action:   "sell",
					DeltaQty: -sellQty,
					Price:    price,
					Amount:   price * float64(sellQty),
					Priority: 1,
					Reason:   fmt.Sprintf("止损触发, 浮亏%.1f%%", pos.FloatingPnLPct*100),
					Urgency:  "立即",
				})
			}
			continue
		}

		// 止盈检查
		if pos.FloatingPnLPct > 0.2 {
			sellQty := pos.AvailableQty
			if sellQty > 0 {
				sellList = append(sellList, TradeSuggestionJSON{
					TsCode:   tsCode,
					Name:     s.stockName(tsCode),
					Action:   "sell",
					DeltaQty: -sellQty,
					Price:    price,
					Amount:   price * float64(sellQty),
					Priority: 2,
					Reason:   fmt.Sprintf("止盈触发, 浮盈%.1f%%", pos.FloatingPnLPct*100),
					Urgency:  "今日",
				})
			}
			continue
		}

		// 策略信号卖出
		if hasSignal && sig.Direction == model.DirSell {
			sellQty := pos.AvailableQty
			if sellQty > 0 {
				sellList = append(sellList, TradeSuggestionJSON{
					TsCode:   tsCode,
					Name:     s.stockName(tsCode),
					Action:   "sell",
					DeltaQty: -sellQty,
					Price:    price,
					Amount:   price * float64(sellQty),
					Priority: 3,
					Reason:   "策略信号: " + sig.Reason,
					Urgency:  "今日",
				})
			}
			continue
		}

		// 持有
		suggestion := "继续持有"
		if pos.FloatingPnLPct < -0.03 {
			suggestion = "关注止损位"
		} else if pos.FloatingPnLPct > 0.15 {
			suggestion = "接近止盈"
		}
		holdList = append(holdList, HoldSuggestionJSON{
			TsCode:      tsCode,
			Name:        s.stockName(tsCode),
			Qty:         pos.TotalQty,
			CostPrice:   pos.CostPrice,
			MarketPrice: price,
			FloatingPnL: pos.FloatingPnL,
			Suggestion:  suggestion,
		})
	}

	// 买入信号
	var buyList []TradeSuggestionJSON
	for _, sig := range signals {
		if sig.Direction != model.DirBuy {
			continue
		}

		price := 0.0
		if bar, ok := todayBars[sig.TsCode]; ok && bar.Close > 0 {
			price = bar.Close
		}
		if price <= 0 {
			continue
		}

		targetQty := sig.TargetQty
		if targetQty <= 0 {
			maxAmount := totalAsset * 0.2
			if maxAmount > 0 {
				targetQty = market.RoundLot(int(maxAmount / price))
			}
		}

		if targetQty <= 0 {
			continue
		}

		priority := int(10 - sig.Strength*9)
		if priority < 1 {
			priority = 1
		}

		buyList = append(buyList, TradeSuggestionJSON{
			TsCode:   sig.TsCode,
			Name:     s.stockName(sig.TsCode),
			Action:   "buy",
			DeltaQty: targetQty,
			Price:    price,
			Amount:   price * float64(targetQty),
			Priority: priority,
			Reason:   sig.Reason,
			Urgency:  "今日",
		})
	}

	// 建议现金比例
	cashPct := 0.15

	// 生成理由
	var reasonParts []string
	if len(sellList) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("建议卖出%d只", len(sellList)))
	}
	if len(buyList) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("建议买入%d只", len(buyList)))
	}
	if len(holdList) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("持有%d只", len(holdList)))
	}
	if len(reasonParts) == 0 {
		reasonParts = append(reasonParts, "无明确调仓信号, 建议维持当前持仓")
	}
	reason := strings.Join(reasonParts, "; ")

	return &RebalanceJSON{
		SellList: sellList,
		BuyList:  buyList,
		HoldList: holdList,
		CashPct:  cashPct,
		Reason:   reason,
	}
}

// buildStrategyJSON 构建策略建议 JSON
func (s *Service) buildStrategyJSON(
	date string,
	signals []model.Signal,
	todayBars map[string]*model.Bar,
	portfolio *PortfolioJSON,
) *StrategyJSON {
	// 使用 analysis.AdviseStrategy
	marketBars := make(map[string]*model.Bar)
	for _, code := range []string{"000001.SH", "399001.SZ", "000300.SH"} {
		if bar, ok := todayBars[code]; ok {
			marketBars[code] = bar
		}
	}

	// 简化: 无历史收益率数据, 使用空数组
	strategyPerformances := make(map[string]analysis.StrategyPerformance)
	advice := analysis.AdviseStrategy(date, marketBars, nil, strategyPerformances)

	return &StrategyJSON{
		Recommended: advice.RecommendedStrategy,
		Confidence:  advice.Confidence,
		Reason:      advice.Reason,
		Condition:   advice.MarketCondition,
	}
}

// buildNewsJSON 构建新闻摘要 JSON
func (s *Service) buildNewsJSON() *NewsJSON {
	newsRepo := store.NewNewsRepo(s.db)
	recentNews, err := newsRepo.GetRecent(20)
	if err != nil || len(recentNews) == 0 {
		return &NewsJSON{
			Sentiment:   "中性",
			RelatedNews: []map[string]string{},
		}
	}

	// 使用 analysis.NewsAnalyzer 分析情感
	na := analysis.NewNewsAnalyzer()
	relatedNews := make([]map[string]string, 0, len(recentNews))
	var totalScore float64
	count := 0

	for _, n := range recentNews {
		score := na.SentimentScore(n.Title + " " + n.Content)
		totalScore += score
		count++

		relatedNews = append(relatedNews, map[string]string{
			"title":     n.Title,
			"source":    "",
			"time":      n.Datetime,
			"sentiment": scoreToLabel(score),
		})
	}

	avgScore := 0.0
	if count > 0 {
		avgScore = totalScore / float64(count)
	}
	sentiment := scoreToLabel(avgScore)

	return &NewsJSON{
		Sentiment:   sentiment,
		RelatedNews: relatedNews,
	}
}

// buildActionItems 构建操作清单
func (s *Service) buildActionItems(
	signals []model.Signal,
	portfolio *PortfolioJSON,
	marketSnapshot *MarketSnapshotJSON,
) []ActionItemJSON {
	var items []ActionItemJSON

	// 开盘前检查
	items = append(items, ActionItemJSON{
		Time:     "09:25",
		Action:   "检查",
		Detail:   "查看隔夜新闻、外围市场、集合竞价情况",
		Priority: 1,
	})

	// 开盘后: 卖出优先
	for _, sig := range signals {
		if sig.Direction == model.DirSell {
			items = append(items, ActionItemJSON{
				Time:     "09:30",
				Action:   "卖出",
				TsCode:   sig.TsCode,
				Name:     s.stockName(sig.TsCode),
				Detail:   sig.Reason,
				Priority: 1,
			})
		}
	}

	// 盘中: 告警检查
	if marketSnapshot != nil {
		for _, alarm := range marketSnapshot.Alarms {
			priority := 3
			if alarm["level"] == "danger" {
				priority = 1
			} else if alarm["level"] == "warning" {
				priority = 2
			}
			items = append(items, ActionItemJSON{
				Time:     "盘中",
				Action:   "检查",
				TsCode:   alarm["ts_code"],
				Detail:   alarm["message"],
				Priority: priority,
			})
		}
	}

	// 盘中: 买入
	for _, sig := range signals {
		if sig.Direction == model.DirBuy {
			items = append(items, ActionItemJSON{
				Time:     "盘中",
				Action:   "买入",
				TsCode:   sig.TsCode,
				Name:     s.stockName(sig.TsCode),
				Detail:   sig.Reason,
				Priority: 3,
			})
		}
	}

	// 尾盘
	items = append(items, ActionItemJSON{
		Time:     "14:50",
		Action:   "检查",
		Detail:   "检查未完成订单, 决定是否留仓过夜",
		Priority: 2,
	})

	// 盘后
	items = append(items, ActionItemJSON{
		Time:     "盘后",
		Action:   "检查",
		Detail:   "复盘当日操作, 更新交易日志",
		Priority: 3,
	})

	return items
}

// runStrategy 运行策略产生信号
func (s *Service) runStrategy(
	date string,
	strategyName string,
	bars map[string]*model.Bar,
	positions map[string]*model.Position,
	asset *broker.AssetInfo,
) []model.Signal {
	reg := strategy.DefaultRegistry()
	strat, ok := reg.Get(strategyName)
	if !ok {
		return nil
	}

	universe := make([]string, 0, len(bars))
	for code := range bars {
		universe = append(universe, code)
	}

	barCtx := &strategy.BarContext{
		TradeDate:  date,
		Universe:   universe,
		Bars:       bars,
		Positions:  positions,
		Cash:       asset.Cash,
		TotalAsset: asset.TotalAsset,
		History:    &historyAdapter{},
	}

	if err := strat.Init(context.Background(), nil); err != nil {
		return nil
	}

	signals, err := strat.OnBar(context.Background(), barCtx)
	if err != nil {
		return nil
	}
	return signals
}

// historyAdapter 简化历史数据适配器
type historyAdapter struct{}

func (h *historyAdapter) GetBars(tsCode, endDate string, n int) ([]model.Bar, error) {
	return nil, nil
}

func (h *historyAdapter) GetCloses(tsCode, endDate string, n int) ([]float64, error) {
	return nil, nil
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

// ==================== HTTP Handler ====================

// HandleHealth 健康检查
func (s *Service) HandleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleDaily 每日操盘报告
// 未指定 strategy 参数时, 自动使用动态策略选择器
func (s *Service) HandleDaily(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("20060102")
	}
	strategyName := r.URL.Query().Get("strategy")

	// 动态策略选择: 未指定策略时, 使用 dynamicSelector
	if strategyName == "" && s.dynamicSelector != nil {
		allBars, err := s.barRepo.GetBarsByDate(date)
		if err == nil && len(allBars) > 0 {
			barMap := make(map[string]*model.Bar, len(allBars))
			for i := range allBars {
				barMap[allBars[i].TsCode] = &allBars[i]
			}
			selectedName, switched := s.dynamicSelector.Select(date, barMap)
			strategyName = selectedName
			if switched {
				logger.L().Infof("[动态策略] %s 策略切换为 %s", date, strategyName)
			}
		}
	}
	if strategyName == "" {
		strategyName = "ma_cross"
	}

	report, err := s.RunDaily(date, strategyName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// HandlePositions 持仓诊断
func (s *Service) HandlePositions(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("20060102")
	}

	portfolio, err := s.RunPositions(date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, portfolio)
}

// HandleRebalance 调仓建议
func (s *Service) HandleRebalance(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("20060102")
	}
	strategyName := r.URL.Query().Get("strategy")
	if strategyName == "" {
		strategyName = "ma_cross"
	}

	rebalance, err := s.RunRebalance(date, strategyName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rebalance)
}

// HandleNews 新闻舆情
func (s *Service) HandleNews(w http.ResponseWriter, r *http.Request) {
	news, err := s.RunNews()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, news)
}

// HandleStrategy 策略建议
func (s *Service) HandleStrategy(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("20060102")
	}
	strategyName := r.URL.Query().Get("strategy")
	if strategyName == "" {
		strategyName = "ma_cross"
	}

	strat, err := s.RunStrategy(date, strategyName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, strat)
}

// HandleMarket 市场概况
func (s *Service) HandleMarket(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("20060102")
	}

	market, err := s.RunMarket(date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, market)
}

// ==================== JSON 响应工具 ====================

// writeJSON 写入成功 JSON 响应
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	resp := APIResponse{
		Code: 0,
		Msg:  "ok",
		Data: data,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(status)

	body, _ := json.MarshalIndent(resp, "", "  ")
	w.Write(body)
	w.Write([]byte("\n"))
}

// writeError 写入错误 JSON 响应
func writeError(w http.ResponseWriter, status int, msg string) {
	resp := APIResponse{
		Code: -1,
		Msg:  msg,
		Data: nil,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(status)

	body, _ := json.MarshalIndent(resp, "", "  ")
	w.Write(body)
	w.Write([]byte("\n"))
}

// formatDate 格式化日期字符串 20260715 -> 2026-07-15
func formatDate(dateStr string) string {
	if len(dateStr) != 8 {
		return dateStr
	}
	return dateStr[:4] + "-" + dateStr[4:6] + "-" + dateStr[6:8]
}

// parseDateParam 解析日期参数 (去掉可能的横杠)
func parseDateParam(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.ReplaceAll(raw, "-", "")
	return raw
}

// parseIntParam 解析整数参数
func parseIntParam(r *http.Request, key string, defaultVal int) int {
	val := r.URL.Query().Get(key)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return n
}

// parseFloatParam 解析浮点参数
func parseFloatParam(r *http.Request, key string, defaultVal float64) float64 {
	val := r.URL.Query().Get(key)
	if val == "" {
		return defaultVal
	}
	n, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return n
}
// ==================== 仪表盘专用接口 ====================

// HandleKline 处理 GET /api/kline?code=510050.SH&start=20260101&end=20260716
func (s *Service) HandleKline(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "code 参数不能为空")
		return
	}
	start := r.URL.Query().Get("start")
	end := r.URL.Query().Get("end")
	if start == "" {
		start = "20200101"
	}
	if end == "" {
		end = time.Now().Format("20060102")
	}

	bars, err := s.barRepo.GetBars(code, start, end)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if bars == nil {
		bars = []model.Bar{}
	}
	writeJSON(w, http.StatusOK, bars)
}

// HandleSnapshots 处理 GET /api/snapshots?limit=30
func (s *Service) HandleSnapshots(w http.ResponseWriter, r *http.Request) {
	limit := parseIntParam(r, "limit", 30)

	var snaps []model.AccountSnapshot
	query := `SELECT trade_date, total_asset, cash, market_value, pnl, pnl_pct, total_pnl, total_pnl_pct
	          FROM account_snapshot ORDER BY trade_date DESC LIMIT ?`
	if err := s.db.Select(&snaps, query, limit); err != nil {
		snaps = []model.AccountSnapshot{}
	}
	if snaps == nil {
		snaps = []model.AccountSnapshot{}
	}

	// 如果没有历史数据，用当前 portfolio 生成一个实时快照
	if len(snaps) == 0 {
		// 先刷新市值
		date := time.Now().Format("20060102")
		allBars, _ := s.barRepo.GetBarsByDate(date)
		todayBars := make(map[string]*model.Bar)
		for i := range allBars {
			b := &allBars[i]
			todayBars[b.TsCode] = b
		}
		s.brk.UpdateMarketValue(todayBars)

		asset, _ := s.brk.QueryAsset()
		if asset != nil && asset.TotalAsset > 0 {
			var totalPnL, totalPnLPct float64
			portfolioRepo := store.NewPortfolioRepo(s.db)
			initialStr, _ := portfolioRepo.GetMeta("initial_capital")
			if initialStr != "" {
				var ic float64
				fmt.Sscanf(initialStr, "%f", &ic)
				if ic > 0 {
					totalPnL = asset.TotalAsset - ic
					totalPnLPct = totalPnL / ic
				}
			}
			snaps = append(snaps, model.AccountSnapshot{
				TradeDate:   date,
				TotalAsset:  asset.TotalAsset,
				Cash:        asset.Cash,
				MarketValue: asset.MarketValue,
				TotalPnL:    totalPnL,
				TotalPnLPct: totalPnLPct,
			})
		}
	}

	// 反转为升序
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
	}
	writeJSON(w, http.StatusOK, snaps)
}

// HandleDashboard 处理 GET / → 返回嵌入的仪表盘 HTML
func (s *Service) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(web.DashboardHTML)
}
