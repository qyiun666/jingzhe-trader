package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/maintenance"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/internal/store"
)

// ==================== 持仓同步 ====================

// SyncPortfolioRequest 持仓同步请求
type SyncPortfolioRequest struct {
	Positions []SyncPositionItem `json:"positions"` // 持仓列表
	Cash      float64            `json:"cash"`      // 可用现金（可选，默认从现有值推算）
	Overwrite bool               `json:"overwrite"` // true=全量覆盖, false=增量更新（默认true）
}

// SyncPositionItem 单只持仓同步条目
type SyncPositionItem struct {
	TsCode       string  `json:"ts_code"`
	TotalQty     int     `json:"total_qty"`
	AvailableQty int     `json:"available_qty"`
	CostPrice    float64 `json:"cost_price"`
}

// SyncPortfolioResponse 持仓同步响应
type SyncPortfolioResponse struct {
	SyncedCount int      `json:"synced_count"`
	Positions   []string `json:"positions"` // 同步的股票代码列表
	TotalAsset  float64  `json:"total_asset"`
	Cash        float64  `json:"cash"`
}

// HandleSyncPortfolio 处理 POST /api/portfolio/sync
// 全量同步持仓：用户将真实持仓 JSON POST 到此接口
func (s *Service) HandleSyncPortfolio(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	var req SyncPortfolioRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	if len(req.Positions) == 0 {
		writeError(w, http.StatusBadRequest, "持仓列表不能为空")
		return
	}

	// 1. 转换为 store 持仓格式
	storeItems := make([]store.PortfolioSyncItem, 0, len(req.Positions))
	positionMap := make(map[string]*model.Position)
	var names []string

	for _, item := range req.Positions {
		if item.TsCode == "" || item.TotalQty <= 0 {
			continue
		}
		storeItems = append(storeItems, store.PortfolioSyncItem{
			TsCode:       item.TsCode,
			TotalQty:     item.TotalQty,
			AvailableQty: item.AvailableQty,
			CostPrice:    item.CostPrice,
			AvgPrice:     item.CostPrice, // 默认用成本价
		})
		positionMap[item.TsCode] = &model.Position{
			TsCode:       item.TsCode,
			TotalQty:     item.TotalQty,
			AvailableQty: item.AvailableQty,
			CostPrice:    item.CostPrice,
		}
		names = append(names, s.stockName(item.TsCode))
	}

	if len(storeItems) == 0 {
		writeError(w, http.StatusBadRequest, "有效持仓为空")
		return
	}

	// 2. 持久化到数据库
	portRepo := store.NewPortfolioRepo(s.db)
	if err := portRepo.SyncPortfolio(storeItems); err != nil {
		writeError(w, http.StatusInternalServerError, "持仓持久化失败: "+err.Error())
		return
	}

	// 3. 更新内存中的 PaperBroker 持仓
	cash := req.Cash
	if cash <= 0 {
		// 从现有资产推算现金
		asset, _ := s.brk.QueryAsset()
		cash = asset.Cash
	}
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.ImportPositions(positionMap, cash)
	}

	// 4. 记录初始资金到元数据
	portRepo.SetMeta("initial_capital", fmt.Sprintf("%.2f", cash))

	writeJSON(w, http.StatusOK, SyncPortfolioResponse{
		SyncedCount: len(storeItems),
		Positions:   names,
		TotalAsset:  cash, // 初始时总资产约等于现金
		Cash:        cash,
	})
}

// HandleGetPortfolio 处理 GET /api/portfolio
// 获取当前持仓列表（从数据库读取）
func (s *Service) HandleGetPortfolio(w http.ResponseWriter, r *http.Request) {
	portRepo := store.NewPortfolioRepo(s.db)
	positions, err := portRepo.GetAllPositions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 转换为带名称的响应
	type PositionDetail struct {
		TsCode        string  `json:"ts_code"`
		Name          string  `json:"name"`
		TotalQty      int     `json:"total_qty"`
		AvailableQty  int     `json:"available_qty"`
		CostPrice     float64 `json:"cost_price"`
		AvgPrice      float64 `json:"avg_price"`
		MarketPrice   float64 `json:"market_price"`
		MarketValue   float64 `json:"market_value"`
		FloatingPnL   float64 `json:"floating_pnl"`
		FloatingPnLPct float64 `json:"floating_pnl_pct"`
	}

	// 获取最新行情来计算市值
	today := time.Now().Format("20060102")
	bars, _ := s.barRepo.GetBarsByDate(today)
	barMap := make(map[string]float64)
	for _, b := range bars {
		barMap[b.TsCode] = b.Close
	}

	var result []PositionDetail
	for _, p := range positions {
		detail := PositionDetail{
			TsCode:       p.TsCode,
			Name:         s.stockName(p.TsCode),
			TotalQty:     p.TotalQty,
			AvailableQty: p.AvailableQty,
			CostPrice:    p.CostPrice,
			AvgPrice:     p.AvgPrice,
		}
		if close, ok := barMap[p.TsCode]; ok && close > 0 {
			detail.MarketPrice = close
			detail.MarketValue = close * float64(p.TotalQty)
			if p.CostPrice > 0 {
				detail.FloatingPnL = detail.MarketValue - p.CostPrice*float64(p.TotalQty)
				detail.FloatingPnLPct = detail.FloatingPnL / (p.CostPrice * float64(p.TotalQty))
			}
		}
		result = append(result, detail)
	}

	writeJSON(w, http.StatusOK, result)
}

// ==================== 交易反馈 ====================

// TradeConfirmRequest 交易确认请求
type TradeConfirmRequest struct {
	TsCode string  `json:"ts_code"` // 股票代码
	Side   string  `json:"side"`    // "buy" 或 "sell"
	Qty    int     `json:"qty"`     // 成交数量
	Price  float64 `json:"price"`   // 成交价格
}

// TradeConfirmResponse 交易确认响应
type TradeConfirmResponse struct {
	TsCode     string  `json:"ts_code"`
	Name       string  `json:"name"`
	Side       string  `json:"side"`
	Qty        int     `json:"qty"`
	Price      float64 `json:"price"`
	Amount     float64 `json:"amount"`
	Cash       float64 `json:"cash"`        // 更新后现金
	TotalAsset float64 `json:"total_asset"` // 更新后总资产
}

// HandleTradeConfirm 处理 POST /api/trade/confirm
// 用户执行交易后，反馈成交信息，系统更新持仓
func (s *Service) HandleTradeConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	var req TradeConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	// 参数校验
	req.TsCode = strings.TrimSpace(req.TsCode)
	if req.TsCode == "" {
		writeError(w, http.StatusBadRequest, "ts_code 不能为空")
		return
	}
	req.Side = strings.ToLower(strings.TrimSpace(req.Side))
	if req.Side != "buy" && req.Side != "sell" {
		writeError(w, http.StatusBadRequest, "side 必须为 buy 或 sell")
		return
	}
	if req.Qty <= 0 || req.Qty%100 != 0 {
		writeError(w, http.StatusBadRequest, "qty 必须是100的整数倍")
		return
	}
	if req.Price <= 0 {
		writeError(w, http.StatusBadRequest, "price 必须大于0")
		return
	}

	// 确定买卖方向
	side := model.SideBuy
	if req.Side == "sell" {
		side = model.SideSell
	}

	// 1. 更新 PaperBroker 内存持仓
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.RecordTrade(req.TsCode, side, req.Qty, req.Price)
	}

	// 2. 更新数据库持仓
	portRepo := store.NewPortfolioRepo(s.db)
	pos, _ := portRepo.GetPosition(req.TsCode)
	if pos == nil {
		pos = &store.PortfolioSyncItem{} // 买入新股票时 pos 为 nil
	}

	if side == model.SideBuy {
		// 买入: 更新或新增持仓
		newQty := pos.TotalQty + req.Qty
		newCost := pos.CostPrice // 保留原成本
		if newQty > 0 && pos.TotalQty > 0 {
			// 加权平均成本
			oldTotal := pos.CostPrice * float64(pos.TotalQty)
			newTotal := req.Price * float64(req.Qty)
			newCost = (oldTotal + newTotal) / float64(newQty)
		} else if pos.TotalQty == 0 {
			newCost = req.Price
		}
		portRepo.UpsertPosition(store.PortfolioSyncItem{
			TsCode:       req.TsCode,
			TotalQty:     newQty,
			AvailableQty: pos.AvailableQty, // T+1: 今日买入明日可卖
			CostPrice:    newCost,
			AvgPrice:     newCost,
		})
	} else {
		// 卖出: 减少持仓
		newQty := pos.TotalQty - req.Qty
		if newQty <= 0 {
			portRepo.RemovePosition(req.TsCode)
		} else {
			portRepo.UpsertPosition(store.PortfolioSyncItem{
				TsCode:       req.TsCode,
				TotalQty:     newQty,
				AvailableQty: pos.AvailableQty,
				CostPrice:    pos.CostPrice,
				AvgPrice:     pos.AvgPrice,
			})
		}
	}

	// 3. 查询更新后的资产
	asset, _ := s.brk.QueryAsset()

	resp := TradeConfirmResponse{
		TsCode:     req.TsCode,
		Name:       s.stockName(req.TsCode),
		Side:       req.Side,
		Qty:        req.Qty,
		Price:      req.Price,
		Amount:     req.Price * float64(req.Qty),
		Cash:       asset.Cash,
		TotalAsset: asset.TotalAsset,
	}
	writeJSON(w, http.StatusOK, resp)
}

// ==================== 动态策略 ====================

// advisorAdapter 将 analysis.AdviseStrategy 包装为 strategy.StrategyAdvisor 接口
type advisorAdapter struct{}

func (a *advisorAdapter) Advise(date string, indexBars map[string]*model.Bar) *strategy.AdvisorResult {
	advice := analysis.AdviseStrategy(date, indexBars, nil, nil)
	return &strategy.AdvisorResult{
		RecommendedStrategy: advice.RecommendedStrategy,
		MarketCondition:     advice.MarketCondition,
		Confidence:          advice.Confidence,
	}
}

// HandleStrategyStatus 处理 GET /api/strategy/status
// 获取当前动态策略选择器状态
func (s *Service) HandleStrategyStatus(w http.ResponseWriter, r *http.Request) {
	if s.dynamicSelector == nil {
		writeError(w, http.StatusServiceUnavailable, "动态策略选择器未启用")
		return
	}
	writeJSON(w, http.StatusOK, s.dynamicSelector.GetStatus())
}

// HandleStrategySwitch 处理 POST /api/strategy/switch
// 手动切换策略
func (s *Service) HandleStrategySwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		// 也支持从 body 读取
		var body struct {
			Strategy string `json:"strategy"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		name = body.Strategy
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "请指定策略名称")
		return
	}

	reg := strategy.DefaultRegistry()
	if _, ok := reg.Get(name); !ok {
		writeError(w, http.StatusBadRequest, "未知策略: "+name)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "策略已切换为 " + name,
		"strategy": name,
	})
}

// ==================== 系统维护 ====================

// HandleSystemStatus 处理 GET /api/system/status
// 获取系统全面状态（数据新鲜度、持仓数量、运行时间等）
func (s *Service) HandleSystemStatus(w http.ResponseWriter, r *http.Request) {
	if s.updater == nil {
		writeError(w, http.StatusServiceUnavailable, "自动维护器未启用")
		return
	}
	status := s.updater.RunHealthCheck()
	writeJSON(w, http.StatusOK, status)
}

// HandleUpdateData 处理 POST /api/system/update-data
// 手动触发数据更新
func (s *Service) HandleUpdateData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if s.updater == nil {
		writeError(w, http.StatusServiceUnavailable, "自动维护器未启用")
		return
	}
	if err := s.updater.UpdateData(); err != nil {
		writeError(w, http.StatusInternalServerError, "数据更新失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "数据更新成功"})
}

// ==================== LLM 深度新闻分析 ====================

// HandleLLMNews 处理 GET /api/news/llm
// 使用 LLM 深度分析新闻，返回结构化分析结果
// 参数:
//   - limit: 分析新闻条数，默认5，最大20
//   - date:  日期过滤（可选，格式 YYYYMMDD），不传则取最近新闻
//
// 注意: LLM 调用较慢（几秒到几十秒），请耐心等待
func (s *Service) HandleLLMNews(w http.ResponseWriter, r *http.Request) {
	if !s.llmClient.IsEnabled() {
		writeError(w, http.StatusServiceUnavailable, "LLM 未启用，请在配置中设置 llm.enabled=true 和 api_key")
		return
	}

	// 解析 limit 参数，默认 5 条，最大 20 条
	limit := 5
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 20 {
			limit = n
		}
	}

	// 获取新闻列表（最近 n 条）
	newsRepo := store.NewNewsRepo(s.db)
	newsList, err := newsRepo.GetRecent(limit)
	if err != nil || len(newsList) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"total": 0,
			"items": []interface{}{},
		})
		return
	}

	// 如果指定了 date 参数，则按日期过滤
	date := r.URL.Query().Get("date")
	if date != "" {
		date = parseDateParam(date)
		var filtered []model.News
		for _, n := range newsList {
			// datetime 格式通常为 "2026-07-15 09:30:00"，取前 10 位日期部分
			if len(n.Datetime) >= 10 {
				newsDate := strings.ReplaceAll(n.Datetime[:10], "-", "")
				if newsDate == date {
					filtered = append(filtered, n)
				}
			}
		}
		newsList = filtered
	}

	if len(newsList) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"total": 0,
			"items": []interface{}{},
		})
		return
	}

	// 新闻 + LLM 分析结果
	type newsWithAnalysis struct {
		model.News
		Analysis *llm.NewsAnalysis `json:"analysis"`
	}

	var results []newsWithAnalysis
	for i := range newsList {
		analysis, err := s.llmNews.AnalyzeNews(&newsList[i])
		item := newsWithAnalysis{News: newsList[i]}
		if err == nil {
			item.Analysis = analysis
		}
		results = append(results, item)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total": len(results),
		"items": results,
	})
}

// ==================== 扩展初始化 ====================

// initExtensions 初始化扩展功能（在 NewService 中调用）
func (s *Service) initExtensions() {
	// 初始化动态策略选择器
	reg := strategy.DefaultRegistry()
	s.dynamicSelector = strategy.NewDynamicSelector(reg, &advisorAdapter{})

	// 初始化自动维护器
	s.updater = maintenance.NewAutoUpdater(s.cfg, s.db)

	// 尝试从数据库恢复持仓到内存
	s.restorePortfolioFromDB()
}

// restorePortfolioFromDB 从数据库恢复持仓到 PaperBroker
func (s *Service) restorePortfolioFromDB() {
	portRepo := store.NewPortfolioRepo(s.db)
	positions, err := portRepo.GetAllPositions()
	if err != nil || len(positions) == 0 {
		return // 无持仓数据，使用默认空仓
	}

	positionMap := make(map[string]*model.Position)
	for _, p := range positions {
		positionMap[p.TsCode] = &model.Position{
			TsCode:       p.TsCode,
			TotalQty:     p.TotalQty,
			AvailableQty: p.AvailableQty,
			CostPrice:    p.CostPrice,
		}
	}

	// 获取初始资金
	capitalStr, _ := portRepo.GetMeta("initial_capital")
	capital := s.cfg.Backtest.InitialCapital
	if capitalStr != "" {
		if v, err := strconv.ParseFloat(capitalStr, 64); err == nil && v > 0 {
			capital = v
		}
	}

	// 导入到 PaperBroker
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.ImportPositions(positionMap, capital)
	}
}
