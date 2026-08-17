package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// ==================== 持仓同步 ====================

// SyncPortfolioRequest 持仓同步请求
type SyncPortfolioRequest struct {
	Positions []SyncPositionItem `json:"positions"` // 持仓列表
	Cash      float64            `json:"cash"`      // 可用现金（可选，默认从现有值推算）
	Overwrite *bool              `json:"overwrite"` // true=全量覆盖, false=逐条增量更新（缺省true）
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

	// 2. 持久化到数据库: Overwrite=true 全量覆盖, false 逐条 Upsert
	overwrite := req.Overwrite == nil || *req.Overwrite
	portRepo := store.NewPortfolioRepo(s.db)
	if overwrite {
		if err := portRepo.SyncPortfolio(storeItems); err != nil {
			writeError(w, http.StatusInternalServerError, "持仓持久化失败: "+err.Error())
			return
		}
	} else {
		for _, item := range storeItems {
			if err := portRepo.UpsertPosition(item); err != nil {
				writeError(w, http.StatusInternalServerError, "持仓增量更新失败: "+err.Error())
				return
			}
		}
	}

	// 3. 更新内存中的 PaperBroker 持仓
	cash := req.Cash
	if cash <= 0 {
		// 从现有资产推算现金
		asset, _ := s.brk.QueryAsset()
		cash = asset.Cash
	}
	if !overwrite {
		// 增量模式: 内存重建为数据库全量持仓 (含本次未触及的旧持仓)
		positionMap = make(map[string]*model.Position)
		if all, err := portRepo.GetAllPositions(); err == nil {
			for _, p := range all {
				positionMap[p.TsCode] = &model.Position{
					TsCode:       p.TsCode,
					TotalQty:     p.TotalQty,
					AvailableQty: p.AvailableQty,
					TodayBought:  p.TodayBought,
					HighPrice:    p.HighPrice,
					CostPrice:    p.CostPrice,
				}
			}
		}
	}
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.ImportPositions(positionMap, cash)
	}

	// 4. 记录现金到元数据; initial_capital 仅首次设置 (避免覆盖已有值导致总盈亏计算错误)
	portRepo.SetMeta("cash", fmt.Sprintf("%.2f", cash))
	if existing, err := portRepo.GetMeta("initial_capital"); err != nil || existing == "" {
		portRepo.SetMeta("initial_capital", fmt.Sprintf("%.2f", cash))
	}

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
	portRepo := store.NewPortfolioRepo(s.db) // PortfolioRepo 含元数据操作, 暂不提升为共享字段
	positions, err := portRepo.GetAllPositions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 转换为带名称的响应
	type PositionDetail struct {
		TsCode         string  `json:"ts_code"`
		Name           string  `json:"name"`
		TotalQty       int     `json:"total_qty"`
		AvailableQty   int     `json:"available_qty"`
		CostPrice      float64 `json:"cost_price"`
		AvgPrice       float64 `json:"avg_price"`
		MarketPrice    float64 `json:"market_price"`
		MarketValue    float64 `json:"market_value"`
		FloatingPnL    float64 `json:"floating_pnl"`
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

	asset := s.applyTradeToPortfolio(req.TsCode, side, req.Qty, req.Price)

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

// liveSnapshotRunID 实盘账户快照的 run_id (与回测 bt_* 区分)
const liveSnapshotRunID = "live"

// goalAdjustedRisk 返回按季度目标状态调节后的风控配置 (只收紧不放松)
// 第二个返回值: 调整说明 (空=未调整或未启用)
func (s *Service) goalAdjustedRisk(date string) (config.RiskConfig, []string) {
	if s.goalTracker == nil {
		return s.cfg.Risk, nil
	}
	asset := s.getAsset()
	st, err := s.goalTracker.Status(date, asset.TotalAsset)
	if err != nil {
		logger.L().Warnf("[目标跟踪] 状态计算失败, 使用基础风控: %v", err)
		return s.cfg.Risk, nil
	}
	adj, notes := s.goalTracker.AdjustRisk(s.cfg.Risk, st)
	for _, n := range notes {
		logger.L().Infof("[目标跟踪] %s %s", date, n)
	}
	return adj, notes
}

// GoalStatus 返回当前季度目标状态 (供 API 与调度器使用)
func (s *Service) GoalStatus(date string) (*goal.Status, error) {
	if s.goalTracker == nil {
		return nil, fmt.Errorf("目标跟踪未启用 (goal.enabled=false)")
	}
	asset := s.getAsset()
	return s.goalTracker.Status(date, asset.TotalAsset)
}

// HandleGoalStatus 处理 GET /api/goal/status
func (s *Service) HandleGoalStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 GET")
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("20060102")
	}
	st, err := s.GoalStatus(date)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// RecordLiveSnapshot 记录实盘每日账户快照 (收益曲线数据, 供日报/目标跟踪/复盘使用)
// 数据更新成功后调用: 用当日收盘价更新市值 → 计算当日/累计盈亏 → 落 account_snapshot
func (s *Service) RecordLiveSnapshot(date string) error {
	bars, err := s.barRepo.GetBarsByDate(date)
	if err != nil || len(bars) == 0 {
		return fmt.Errorf("无当日行情, 跳过快照: %w", err)
	}
	barMap := make(map[string]*model.Bar, len(bars))
	for i := range bars {
		barMap[bars[i].TsCode] = &bars[i]
	}
	s.brk.UpdateMarketValue(barMap)
	asset, err := s.brk.QueryAsset()
	if err != nil {
		return fmt.Errorf("查询资产失败: %w", err)
	}

	snap := model.AccountSnapshot{
		TradeDate:   date,
		TotalAsset:  asset.TotalAsset,
		Cash:        asset.Cash,
		MarketValue: asset.MarketValue,
	}
	tradeRepo := store.NewTradeRepo(s.db)
	// 当日盈亏: 对比上一个实盘快照
	if prev, err := tradeRepo.GetLatestAccountSnapshot(liveSnapshotRunID); err == nil && prev != nil && prev.TotalAsset > 0 {
		snap.PnL = snap.TotalAsset - prev.TotalAsset
		snap.PnLPct = snap.PnL / prev.TotalAsset
	}
	// 累计盈亏: 对比初始资金
	portRepo := store.NewPortfolioRepo(s.db)
	initial := s.cfg.Backtest.InitialCapital
	if v, _ := portRepo.GetMeta("initial_capital"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			initial = f
		}
	}
	if initial > 0 {
		snap.TotalPnL = snap.TotalAsset - initial
		snap.TotalPnLPct = snap.TotalPnL / initial
	}
	if err := tradeRepo.InsertAccountSnapshot(liveSnapshotRunID, snap); err != nil {
		return fmt.Errorf("快照落库失败: %w", err)
	}
	logger.L().Infof("[实盘快照] %s 总资产 %.2f 当日盈亏 %.2f (%.2f%%) 累计 %.2f%%",
		date, snap.TotalAsset, snap.PnL, snap.PnLPct*100, snap.TotalPnLPct*100)
	return nil
}

// PollBrokerTrades 轮询券商端成交回报 (QMT 模式由对账任务调用, paper 模式无操作)
func (s *Service) PollBrokerTrades() error {
	if qb, ok := s.brk.(*broker.QMTBridge); ok {
		return qb.PollTrades()
	}
	return nil
}

// SettleT1 每日开盘前结转: 内存 broker 与 DB 持仓的 T+1 交收 (昨日买入转为可卖)
func (s *Service) SettleT1(date string) error {
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.SettleT1(date)
	}
	if err := store.NewPortfolioRepo(s.db).SettleT1(); err != nil {
		return err
	}
	logger.L().Infof("[T+1结算] %s 持仓结转完成", date)
	return nil
}

// applyTradeToPortfolio 将成交同步到内存持仓与数据库 (trade/confirm 与 plan/confirm 共用)
func (s *Service) applyTradeToPortfolio(tsCode string, side model.Side, qty int, price float64) *broker.AssetInfo {
	// 1. 更新 PaperBroker 内存持仓
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.RecordTrade(tsCode, side, qty, price)
	}

	// 2. 更新数据库持仓
	portRepo := store.NewPortfolioRepo(s.db)
	pos, _ := portRepo.GetPosition(tsCode)
	if pos == nil {
		pos = &store.PortfolioSyncItem{} // 买入新股票时 pos 为 nil
	}

	if side == model.SideBuy {
		// 买入: 更新或新增持仓, 加权平均成本
		// T+1: 今日买入计入 today_bought, 不计入可卖量, 次日开盘结转后可卖
		newQty := pos.TotalQty + qty
		newCost := pos.CostPrice
		if newQty > 0 && pos.TotalQty > 0 {
			oldTotal := pos.CostPrice * float64(pos.TotalQty)
			newCost = (oldTotal + price*float64(qty)) / float64(newQty)
		} else if pos.TotalQty == 0 {
			newCost = price
		}
		highPrice := pos.HighPrice
		if price > highPrice {
			highPrice = price // 买入价也可能是持仓期新高
		}
		portRepo.UpsertPosition(store.PortfolioSyncItem{
			TsCode:       tsCode,
			TotalQty:     newQty,
			AvailableQty: pos.AvailableQty, // T+1: 今日买入明日可卖
			TodayBought:  pos.TodayBought + qty,
			HighPrice:    highPrice,
			CostPrice:    newCost,
			AvgPrice:     newCost,
		})
	} else {
		// 卖出: 减少持仓与可卖量, 清仓则删除记录
		newQty := pos.TotalQty - qty
		newAvail := pos.AvailableQty - qty
		if newAvail < 0 {
			newAvail = 0
		}
		if newQty <= 0 {
			portRepo.RemovePosition(tsCode)
		} else {
			portRepo.UpsertPosition(store.PortfolioSyncItem{
				TsCode:       tsCode,
				TotalQty:     newQty,
				AvailableQty: newAvail,
				TodayBought:  pos.TodayBought,
				HighPrice:    pos.HighPrice,
				CostPrice:    pos.CostPrice,
				AvgPrice:     pos.AvgPrice,
			})
		}
	}

	// 3. 成交落库 (绩效归因/收益曲线的数据源, 与回测 trades 同表区分 run_id)
	if _, err := store.NewTradeRepo(s.db).InsertTrade(&model.Trade{
		RunID:     liveSnapshotRunID,
		TsCode:    tsCode,
		Side:      side,
		Price:     price,
		Qty:       qty,
		Amount:    price * float64(qty),
		TradeDate: time.Now().Format("20060102"),
		TradeTime: time.Now().Format("20060102 150405"),
	}); err != nil {
		logger.L().Warnw("成交落库失败", "ts_code", tsCode, "err", err)
	}

	// 4. 查询更新后的资产并持久化 cash
	asset, _ := s.brk.QueryAsset()
	if asset != nil {
		portRepo.SetMeta("cash", fmt.Sprintf("%.2f", asset.Cash))
	} else {
		asset = &broker.AssetInfo{}
	}
	return asset
}

// ==================== 动态策略 ====================

// advisorAdapter 将 analysis.AdviseStrategy 包装为 strategy.StrategyAdvisor 接口
// 输入真实数据:
//   - 沪深300近30个交易日收益序列 (市场环境判断基于趋势而非单日涨跌)
//   - 各策略最近一次回测run的绩效 (夏普/胜率/回撤, 来自 trades 归因, 缓存1小时)
type advisorAdapter struct {
	barRepo   *store.BarRepo
	tradeRepo *store.TradeRepo
	mu        sync.Mutex
	cache     map[string]analysis.StrategyPerformance
	cachedAt  time.Time
}

func (a *advisorAdapter) Advise(date string, indexBars map[string]*model.Bar) *strategy.AdvisorResult {
	recentReturns := a.recentIndexReturns(date)
	advice := analysis.AdviseStrategy(date, indexBars, recentReturns, a.strategyPerformances())
	return &strategy.AdvisorResult{
		RecommendedStrategy: advice.RecommendedStrategy,
		MarketCondition:     advice.MarketCondition,
		Confidence:          advice.Confidence,
	}
}

// strategyPerformances 各策略最近一次回测run的真实绩效 (缓存1小时, 回测数据静态)
func (a *advisorAdapter) strategyPerformances() map[string]analysis.StrategyPerformance {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.cachedAt) < time.Hour && a.cache != nil {
		return a.cache
	}
	out := make(map[string]analysis.StrategyPerformance)
	if a.tradeRepo != nil {
		runs, err := a.tradeRepo.GetLatestRunPerStrategy()
		if err != nil {
			logger.L().Warnw("策略业绩查询失败, 使用中性基准", "err", err)
		} else {
			for strat, runID := range runs {
				snaps, err1 := a.tradeRepo.GetAccountSnapshotsByRunID(runID)
				trades, err2 := a.tradeRepo.GetTradesByRunID(runID)
				if err1 != nil || err2 != nil || len(snaps) < 2 || len(trades) == 0 {
					continue
				}
				m := backtest.CalculateMetrics(snaps, trades, nil)
				out[strat] = analysis.StrategyPerformance{
					Name:        strat,
					TotalReturn: m.TotalReturn,
					Sharpe:      m.SharpeRatio,
					MaxDrawdown: m.MaxDrawdown,
					WinRate:     m.WinRate,
				}
			}
		}
	}
	a.cache = out
	a.cachedAt = time.Now()
	return out
}

// recentIndexReturns 沪深300最近30个交易日的日收益率序列 (前复权口径, 与策略一致)
func (a *advisorAdapter) recentIndexReturns(date string) []float64 {
	if a == nil || a.barRepo == nil {
		return nil
	}
	bars, err := a.barRepo.GetBars("000300.SH", "", date)
	if err != nil {
		return nil
	}
	if len(bars) < 2 {
		return nil
	}
	// 取最近30根
	if len(bars) > 30 {
		bars = bars[len(bars)-30:]
	}
	// 前复权后再算收益率, 与 DataProvider 同口径
	model.AdjustBarsForward(bars)
	returns := make([]float64, 0, len(bars)-1)
	for i := 1; i < len(bars); i++ {
		if bars[i-1].Close > 0 {
			returns = append(returns, bars[i].Close/bars[i-1].Close-1)
		}
	}
	return returns
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
// 手动切换策略: 更新动态选择器的当前策略 + 刷新策略缓存
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

	// 验证策略存在并确保缓存中有实例
	strat, ok := s.getStrategy(name)
	if !ok {
		writeError(w, http.StatusBadRequest, "未知策略: "+name)
		return
	}

	// 通过动态选择器执行切换
	if s.dynamicSelector != nil {
		s.dynamicSelector.SwitchTo(name, strat)
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "策略已切换为 " + name,
		"strategy": name,
	})
}

// ==================== 系统维护 ====================

// SystemStatus 系统状态
type SystemStatus struct {
	Healthy        bool   `json:"healthy"`
	LastDataDate   string `json:"last_data_date"` // 数据库中最新的行情日期
	Today          string `json:"today"`
	DataFresh      bool   `json:"data_fresh"` // 数据是否是最新的
	Uptime         string `json:"uptime"`
	PortfolioCount int    `json:"portfolio_count"`  // 持仓数量
	NextMarketOpen string `json:"next_market_open"` // 下一个交易日
}

// HandleSystemStatus 处理 GET /api/system/status
// 获取系统全面状态（数据新鲜度、持仓数量、运行时间等）
func (s *Service) HandleSystemStatus(w http.ResponseWriter, r *http.Request) {
	status := SystemStatus{
		Healthy: true,
		Today:   time.Now().Format("20060102"),
		Uptime:  time.Since(s.startTime).Truncate(time.Second).String(),
	}
	if err := s.db.Ping(); err != nil {
		status.Healthy = false
		writeJSON(w, http.StatusOK, status)
		return
	}
	if maxDate, err := s.barRepo.GetMaxTradeDate(); err == nil {
		status.LastDataDate = maxDate
		if preDate, perr := s.calRepo.GetPreTradeDate(status.Today); perr == nil && preDate != "" {
			status.DataFresh = maxDate >= preDate
		}
	}
	if positions, err := store.NewPortfolioRepo(s.db).GetAllPositions(); err == nil { // 同上
		status.PortfolioCount = len(positions)
	}
	if nextDate, err := s.calRepo.GetNextTradeDate(status.Today); err == nil {
		status.NextMarketOpen = nextDate
	}
	writeJSON(w, http.StatusOK, status)
}

// HandleUpdateData 处理 POST /api/system/update-data
// 手动触发数据更新 (进程内调用 dataloader, 不再 exec 二进制)
func (s *Service) HandleUpdateData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	if err := s.UpdateData(); err != nil {
		writeError(w, http.StatusInternalServerError, "数据更新失败: "+err.Error())
		return
	}
	// 数据更新后刷新股票名称缓存
	s.loadStockMap()
	writeJSON(w, http.StatusOK, map[string]string{"message": "数据更新成功"})
}

// UpdateData 进程内执行增量数据更新 (从库内最新日期补到今天); 同一时刻只允许一个更新任务
// 非阻塞: 如果有其他更新在执行, 立即返回错误
func (s *Service) UpdateData() error {
	if !s.updateMu.TryLock() {
		return fmt.Errorf("数据更新任务正在执行中, 请稍后重试")
	}
	defer s.updateMu.Unlock()
	return s.doUpdateData()
}

// UpdateDataBlocking 阻塞版数据更新: 等待其他更新完成后执行 (供信号任务前置调用)
func (s *Service) UpdateDataBlocking() error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	return s.doUpdateData()
}

// doUpdateData 增量数据更新核心逻辑
func (s *Service) doUpdateData() error {
	opts := dataloader.Options{}
	if maxDate, err := s.barRepo.GetMaxTradeDate(); err == nil && maxDate != "" {
		opts.StartDate = maxDate // 增量: 从库内最新日期补起 (含当日, 幂等覆盖)
	}
	return dataloader.New(s.cfg, s.db).Run(opts)
}

// SyncCalendar 仅同步交易日历 (轻量级, 供调度器打破日历死锁)
// 同步近一周到后一周的日历数据, 确保今天和近期日期都在日历中
func (s *Service) SyncCalendar() error {
	start := time.Now().AddDate(0, 0, -7).Format("20060102")
	end := time.Now().AddDate(0, 0, 7).Format("20060102")
	return dataloader.New(s.cfg, s.db).SyncCalendarOnly(start, end)
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
	// 初始化动态策略选择器 (advisor 带真实指数收益序列)
	reg := strategy.DefaultRegistry()
	s.dynamicSelector = strategy.NewDynamicSelector(reg, &advisorAdapter{
		barRepo:   s.barRepo,
		tradeRepo: store.NewTradeRepo(s.db),
	})

	// 预热策略缓存: 为每个已注册策略创建并初始化实例, 避免运行时重建丢失内部状态
	for _, name := range reg.Names() {
		strat, ok := reg.Get(name)
		if !ok {
			continue
		}
		if err := strat.Init(context.Background(), s.cfg.StrategyParams(name)); err != nil {
			continue
		}
		s.strategyCache[name] = strat
	}

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
			TodayBought:  p.TodayBought,
			HighPrice:    p.HighPrice,
			CostPrice:    p.CostPrice,
		}
	}

	// 优先读取实际 cash，其次 initial_capital，最后 fallback 到 config
	cash := s.cfg.Backtest.InitialCapital
	if cashStr, _ := portRepo.GetMeta("cash"); cashStr != "" {
		if v, err := strconv.ParseFloat(cashStr, 64); err == nil && v > 0 {
			cash = v
		}
	} else if capitalStr, _ := portRepo.GetMeta("initial_capital"); capitalStr != "" {
		if v, err := strconv.ParseFloat(capitalStr, 64); err == nil && v > 0 {
			cash = v
		}
	}

	// 导入到 PaperBroker
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.ImportPositions(positionMap, cash)
	}
}
