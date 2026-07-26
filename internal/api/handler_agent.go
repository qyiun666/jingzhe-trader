package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/report"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// ==================== 策略选择与交易计划生成 ====================

// SelectStrategy 选择当日策略: 优先动态选择器, 兜底 ma_cross
func (s *Service) SelectStrategy(date string) string {
	if s.dynamicSelector != nil {
		allBars, err := s.barRepo.GetBarsByDate(date)
		if err == nil && len(allBars) > 0 {
			barMap := make(map[string]*model.Bar, len(allBars))
			for i := range allBars {
				barMap[allBars[i].TsCode] = &allBars[i]
			}
			if name, switched := s.dynamicSelector.Select(date, barMap); name != "" {
				if switched {
					logger.L().Infof("[动态策略] %s 策略切换为 %s", date, name)
				}
				return name
			}
		}
	}
	return "ma_cross"
}

// GenerateTradePlans 生成指定日期的交易计划
// 流程: 全局止损信号 + 策略信号 → 风控过滤 → TradePlan (与执行管道同一条信号链路)
func (s *Service) GenerateTradePlans(date string) ([]*store.TradePlan, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取 %s 行情失败: %w", date, err)
	}
	if len(allBars) == 0 {
		return nil, fmt.Errorf("当日 %s 无行情数据", date)
	}
	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		todayBars[allBars[i].TsCode] = &allBars[i]
	}

	s.brk.UpdateMarketValue(todayBars)
	positions := s.getPositions()
	asset := s.getAsset()

	// 风控管理器: 与回测/实盘同一套配置
	rm := risk.NewRiskManager(s.cfg.Risk)
	rm.SetSizeLimits(risk.SizeLimits{
		MinTradeAmount: s.cfg.Trading.MinTradeAmount,
		MaxPositions:   s.cfg.Trading.MaxPositions,
		MinCommission:  s.cfg.Cost.MinCommission,
	})

	// 止损信号优先, 策略信号对同一股票的信号剔除
	stopSignals := rm.CheckStopLoss(positions, todayBars)
	stopCodes := make(map[string]bool, len(stopSignals))
	for _, sig := range stopSignals {
		stopCodes[sig.TsCode] = true
	}
	strategyName := s.SelectStrategy(date)
	merged := append([]model.Signal{}, stopSignals...)
	for _, sig := range s.runStrategy(date, strategyName, todayBars, positions, asset) {
		if !stopCodes[sig.TsCode] {
			merged = append(merged, sig)
		}
	}

	passed, rejections := rm.Check(merged, positions, asset.TotalAsset, s.loadRiskStocks(merged), date, todayBars)
	for _, rej := range rejections {
		logger.L().Infof("[计划生成] 风控拦截 %s: %s (%s)", rej.TsCode, rej.Reason, rej.Rule)
	}

	return s.signalsToPlans(date, strategyName, passed, todayBars, stopCodes), nil
}

// loadRiskStocks 加载信号涉及股票的基本信息 (风控黑名单/ST过滤用)
func (s *Service) loadRiskStocks(signals []model.Signal) map[string]*model.Stock {
	stockRepo := store.NewStockRepo(s.db)
	stocks := make(map[string]*model.Stock, len(signals))
	for _, sig := range signals {
		if _, ok := stocks[sig.TsCode]; ok {
			continue
		}
		if st, err := stockRepo.GetByCode(sig.TsCode); err == nil && st != nil {
			stocks[sig.TsCode] = st
		} else {
			stocks[sig.TsCode] = &model.Stock{TsCode: sig.TsCode, ListStatus: "L"}
		}
	}
	return stocks
}

// signalsToPlans 将通过风控的信号转换为交易计划
func (s *Service) signalsToPlans(date, strategyName string, signals []model.Signal,
	bars map[string]*model.Bar, stopCodes map[string]bool) []*store.TradePlan {

	plans := make([]*store.TradePlan, 0, len(signals))
	for _, sig := range signals {
		direction := "buy"
		if sig.Direction == model.DirSell {
			direction = "sell"
		}
		refPrice := 0.0
		if bar := bars[sig.TsCode]; bar != nil {
			refPrice = bar.Close
		}
		urgency := store.PlanUrgencyNormal
		if stopCodes[sig.TsCode] {
			urgency = store.PlanUrgencyUrgent
		}
		plans = append(plans, &store.TradePlan{
			TradeDate: date,
			TsCode:    sig.TsCode,
			Name:      s.stockName(sig.TsCode),
			Direction: direction,
			Qty:       sig.TargetQty,
			RefPrice:  refPrice,
			Reason:    sig.Reason,
			Strategy:  strategyName,
			Urgency:   urgency,
		})
	}
	return plans
}

// ==================== Agent 专用接口 ====================

// AgentBrief Agent 单次调用所需的全量上下文
type AgentBrief struct {
	Date         string             `json:"date"`           // 数据基准日期
	DataLastDate string             `json:"data_last_date"` // 数据库最新行情日期
	DataFresh    bool               `json:"data_fresh"`     // 数据是否新鲜
	OpenPlans    []store.TradePlan  `json:"open_plans"`     // 待处理的交易计划
	Portfolio    *PortfolioJSON     `json:"portfolio"`      // 持仓诊断
	Market       *MarketSnapshotJSON `json:"market"`        // 市场概况
	Jobs         map[string]string  `json:"jobs"`           // 各任务最近成功时间
	Warnings     []string           `json:"warnings"`       // 数据/任务异常提示
}

// HandleAgentBrief GET /api/agent/brief
// 一次返回 Agent 决策所需全部上下文
func (s *Service) HandleAgentBrief(w http.ResponseWriter, r *http.Request) {
	brief := &AgentBrief{Jobs: map[string]string{}, Warnings: []string{}}

	// 数据新鲜度
	lastDate, err := s.barRepo.GetMaxTradeDate()
	if err != nil || lastDate == "" {
		brief.Warnings = append(brief.Warnings, "数据库无行情数据, 请先执行数据更新")
		writeJSON(w, http.StatusOK, brief)
		return
	}
	brief.Date = lastDate
	brief.DataLastDate = lastDate
	today := time.Now().Format("20060102")
	if preDate, perr := s.calRepo.GetPreTradeDate(today); perr == nil && preDate != "" {
		brief.DataFresh = lastDate >= preDate
	}
	if !brief.DataFresh {
		brief.Warnings = append(brief.Warnings, "行情数据不是最新, 计划参考价可能过期")
	}

	// 待处理计划
	planRepo := store.NewPlanRepo(s.db)
	if plans, perr := planRepo.GetOpenPlans(); perr == nil {
		brief.OpenPlans = plans
	} else {
		brief.Warnings = append(brief.Warnings, "查询交易计划失败: "+perr.Error())
	}

	// 持仓诊断与市场概况 (尽力而为)
	if portfolio, perr := s.RunPositions(lastDate); perr == nil {
		brief.Portfolio = portfolio
	}
	if market, merr := s.RunMarket(lastDate); merr == nil {
		brief.Market = market
	}

	// 任务健康度
	jobRepo := store.NewJobRepo(s.db)
	for _, name := range []string{"data_update", "signal", "report", "intraday_monitor", "retention"} {
		if run, jerr := jobRepo.LastSuccess(name); jerr == nil && run != nil {
			brief.Jobs[name] = run.FinishedAt
		}
	}

	writeJSON(w, http.StatusOK, brief)
}

// HandlePlanList GET /api/plan?date=YYYYMMDD
// 查询交易计划列表, 不传 date 时返回全部待处理计划
func (s *Service) HandlePlanList(w http.ResponseWriter, r *http.Request) {
	planRepo := store.NewPlanRepo(s.db)
	date := r.URL.Query().Get("date")

	var plans []store.TradePlan
	var err error
	if date != "" {
		plans, err = planRepo.GetPlansByDate(parseDateParam(date))
	} else {
		plans, err = planRepo.GetOpenPlans()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plans)
}

// PlanConfirmRequest 计划确认请求
type PlanConfirmRequest struct {
	ID int64 `json:"id"` // 交易计划ID
}

// HandlePlanConfirm POST /api/plan/confirm
// 确认交易计划; auto_execute=true 且 broker=qmt 时直接经QMT下单
func (s *Service) HandlePlanConfirm(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	var req PlanConfirmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "JSON 解析失败: "+err.Error())
		return
	}

	planRepo := store.NewPlanRepo(s.db)
	plan, err := planRepo.GetPlanByID(req.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if plan.Status != store.PlanStatusPending {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("计划状态为 %s, 无法确认", plan.Status))
		return
	}

	status := store.PlanStatusConfirmed
	// 自动执行: 仅 QMT 实盘模式下真实下单; 下单结果飞书推送
	if s.cfg.Trading.AutoExecute && s.cfg.Broker.Type == "qmt" {
		notifier := notify.NewFeishuNotifier(s.cfg.Feishu.WebhookURL)
		if err := s.executePlanViaQMT(plan); err != nil {
			if nerr := notifier.SendText(fmt.Sprintf("❌ 惊蛰下单失败\n%s %s %d股 @%.2f: %v",
				plan.TsCode, plan.Direction, plan.Qty, plan.RefPrice, err)); nerr != nil {
				logger.L().Warnw("飞书通知发送失败", "err", nerr)
			}
			writeError(w, http.StatusInternalServerError, "QMT下单失败: "+err.Error())
			return
		}
		if nerr := notifier.SendText(fmt.Sprintf("✅ 惊蛰下单成功\n%s %s %d股 @%.2f (%s)",
			plan.TsCode, plan.Direction, plan.Qty, plan.RefPrice, plan.Reason)); nerr != nil {
			logger.L().Warnw("飞书通知发送失败", "err", nerr)
		}
		status = store.PlanStatusExecuted
	}

	if err := planRepo.UpdatePlanStatus(plan.ID, status); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	plan.Status = status
	writeJSON(w, http.StatusOK, plan)
}

// executePlanViaQMT 通过 QMT 桥执行交易计划, 成功后同步本地持仓
func (s *Service) executePlanViaQMT(plan *store.TradePlan) error {
	bridge := broker.NewQMTBridge(s.cfg.Broker.QMT.URL)
	side := model.SideBuy
	if plan.Direction == "sell" {
		side = model.SideSell
	}
	if _, err := bridge.PlaceOrder(broker.OrderRequest{
		TsCode:   plan.TsCode,
		Side:     side,
		Qty:      plan.Qty,
		Price:    plan.RefPrice,
		Reason:   plan.Reason,
		Strategy: plan.Strategy,
	}); err != nil {
		return fmt.Errorf("下单失败: %w", err)
	}
	// 同步本地持仓记录 (与 /api/trade/confirm 同一逻辑)
	s.applyTradeToPortfolio(plan.TsCode, side, plan.Qty, plan.RefPrice)
	return nil
}

// HandleReconcile GET /api/reconcile?date=YYYYMMDD
// 对账: 本地记录 vs 券商(QMT)/模拟账户
func (s *Service) HandleReconcile(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().Format("20060102")
	} else {
		date = parseDateParam(date)
	}

	brk := s.brk
	if s.cfg.Broker.Type == "qmt" {
		brk = broker.NewQMTBridge(s.cfg.Broker.QMT.URL)
	}
	result, err := report.Reconcile(brk, store.NewTradeRepo(s.db), date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "对账失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"result": result,
		"report": report.GenerateReconcileReport(result),
	})
}

// ==================== 健康检查扩展 ====================

// HealthStatus /api/health 响应
type HealthStatus struct {
	Status       string            `json:"status"`
	Uptime       string            `json:"uptime"`
	Goroutines   int               `json:"goroutines"`
	DBSizeBytes  int64             `json:"db_size_bytes"`
	DataLastDate string            `json:"data_last_date"`
	DataFresh    bool              `json:"data_fresh"`
	Jobs         map[string]string `json:"jobs"` // 各任务最近成功时间
}

// BuildHealthStatus 构建健康状态 (含运行时指标与任务健康度)
func (s *Service) BuildHealthStatus() *HealthStatus {
	hs := &HealthStatus{
		Status:     "ok",
		Uptime:     time.Since(s.startTime).Truncate(time.Second).String(),
		Goroutines: runtime.NumGoroutine(),
		Jobs:       map[string]string{},
	}
	if err := s.db.Ping(); err != nil {
		hs.Status = "db_error"
		return hs
	}
	if info, err := os.Stat(s.cfg.Database.Path); err == nil {
		hs.DBSizeBytes = info.Size()
	}
	if lastDate, err := s.barRepo.GetMaxTradeDate(); err == nil {
		hs.DataLastDate = lastDate
		today := time.Now().Format("20060102")
		if preDate, perr := s.calRepo.GetPreTradeDate(today); perr == nil && preDate != "" {
			hs.DataFresh = lastDate >= preDate
		}
	}
	jobRepo := store.NewJobRepo(s.db)
	for _, name := range []string{"data_update", "signal", "report", "intraday_monitor", "retention"} {
		if run, err := jobRepo.LastSuccess(name); err == nil && run != nil {
			hs.Jobs[name] = run.FinishedAt
		}
	}
	return hs
}
