package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	"jingzhe-trader/internal/agent"
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

	// 智能体辩论增强 (LLM可用时对买入信号跑辩论)
	if s.debateOrchestrator != nil && s.debateOrchestrator.IsEnabled() {
		merged = s.debateOrchestrator.EnhanceSignals(date, merged, todayBars, positions, asset.TotalAsset, s.stockMap)
	}

	passed, rejections := rm.Check(merged, positions, asset.TotalAsset, s.loadRiskStocks(merged), date, todayBars)
	for _, rej := range rejections {
		logger.L().Infof("[计划生成] 风控拦截 %s: %s (%s)", rej.TsCode, rej.Reason, rej.Rule)
	}

	// 卖出优先排序 (先卖释放资金再买), 与 engine.Pipeline 保持一致
	sellFirstSort(passed)

	return s.signalsToPlans(date, strategyName, passed, todayBars, stopCodes), nil
}

// sellFirstSort 卖出信号排前, 买入排后 (原地排序)
func sellFirstSort(signals []model.Signal) {
	for i := 0; i < len(signals); i++ {
		for j := i + 1; j < len(signals); j++ {
			if signals[i].Direction == model.DirBuy && signals[j].Direction == model.DirSell {
				signals[i], signals[j] = signals[j], signals[i]
			}
		}
	}
}

// loadRiskStocks 加载信号涉及股票的基本信息 (风控黑名单/ST过滤用)
func (s *Service) loadRiskStocks(signals []model.Signal) map[string]*model.Stock {
	stocks := make(map[string]*model.Stock, len(signals))
	for _, sig := range signals {
		if _, ok := stocks[sig.TsCode]; ok {
			continue
		}
		if st, err := s.stockRepo.GetByCode(sig.TsCode); err == nil && st != nil {
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
	Date             string                     `json:"date"`               // 数据基准日期
	DataLastDate     string                     `json:"data_last_date"`     // 数据库最新行情日期
	DataFresh        bool                       `json:"data_fresh"`         // 数据是否新鲜
	OpenPlans        []store.TradePlan          `json:"open_plans"`         // 待处理的交易计划
	Portfolio        *PortfolioJSON             `json:"portfolio"`          // 持仓诊断
	Market           *MarketSnapshotJSON        `json:"market"`             // 市场概况
	Jobs             map[string]string          `json:"jobs"`               // 各任务最近成功时间
	Warnings         []string                   `json:"warnings"`           // 数据/任务异常提示
	Debates          []store.DebateResult       `json:"debates"`            // 当日辩论结果
	ScreenResults    []store.ScreenResult       `json:"screen_results"`     // 最新选股结果
	ActionNeeded     []string                   `json:"action_needed"`      // 需要用户操作的提示
	DecisionChanges  []agent.DecisionChange     `json:"decision_changes"`   // 决策变更记录
	PlanStatusSummary PlanStatusSummary         `json:"plan_status_summary"` // 交易计划状态汇总
	TaskCompleted    map[string]bool            `json:"task_completed"`     // 当日各任务是否已完成
}

// PlanStatusSummary 交易计划状态汇总
type PlanStatusSummary struct {
	Pending   int `json:"pending"`    // 待确认
	Confirmed int `json:"confirmed"`  // 已确认待执行
	Executed  int `json:"executed"`   // 已执行
	Expired   int `json:"expired"`    // 已过期
	Total     int `json:"total"`      // 总计
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
	if plans, perr := s.planRepo.GetOpenPlans(); perr == nil {
		brief.OpenPlans = plans
	} else {
		brief.Warnings = append(brief.Warnings, "查询交易计划失败: "+perr.Error())
	}

	// 辩论结果
	if debates, derr := s.debateRepo.GetByDate(lastDate); derr == nil {
		brief.Debates = debates
	}

	// 选股结果
	if s.screenRepo != nil {
		if screenResults, err := s.screenRepo.GetLatest(); err == nil {
			brief.ScreenResults = screenResults
		}
	}

	// 持仓诊断与市场概况 (尽力而为)
	if portfolio, perr := s.RunPositions(lastDate); perr == nil {
		brief.Portfolio = portfolio
	}
	if market, merr := s.RunMarket(lastDate); merr == nil {
		brief.Market = market
	}

	// 任务健康度
	for _, name := range []string{"data_update", "signal", "report", "intraday_monitor", "retention"} {
		if run, jerr := s.jobRepo.LastSuccess(name); jerr == nil && run != nil {
			brief.Jobs[name] = run.FinishedAt
		}
	}

	// 决策变更检测
	if s.debateOrchestrator != nil && s.debateOrchestrator.IsEnabled() && len(brief.Debates) > 0 {
		brief.DecisionChanges = s.debateOrchestrator.DetectDecisionChanges(brief.Debates)
	}

	// 交易计划状态汇总
	brief.PlanStatusSummary = s.buildPlanStatusSummary(brief.OpenPlans)

	// 当日任务完成状态
	brief.TaskCompleted = s.buildTaskCompletedStatus(today)

	// 需要用户操作的提示
	brief.ActionNeeded = s.buildActionNeededEnhanced(brief.OpenPlans, brief.DecisionChanges, brief.PlanStatusSummary)

	writeJSON(w, http.StatusOK, brief)
}

// buildPlanStatusSummary 构建交易计划状态汇总
func (s *Service) buildPlanStatusSummary(plans []store.TradePlan) PlanStatusSummary {
	summary := PlanStatusSummary{Total: len(plans)}
	for _, p := range plans {
		switch p.Status {
		case store.PlanStatusPending:
			summary.Pending++
		case store.PlanStatusConfirmed:
			summary.Confirmed++
		case store.PlanStatusExecuted:
			summary.Executed++
		case store.PlanStatusExpired:
			summary.Expired++
		}
	}
	return summary
}

// buildTaskCompletedStatus 构建当日任务完成状态
func (s *Service) buildTaskCompletedStatus(today string) map[string]bool {
	status := map[string]bool{}
	for _, name := range []string{"data_update", "signal", "report", "intraday_monitor", "retention"} {
		done, err := s.jobRepo.HasSucceeded(name, today)
		if err == nil {
			status[name] = done
		} else {
			status[name] = false
		}
	}
	return status
}

// buildActionNeededEnhanced 增强版操作提示 (包含决策变更)
func (s *Service) buildActionNeededEnhanced(plans []store.TradePlan, changes []agent.DecisionChange, summary PlanStatusSummary) []string {
	var actions []string

	// 待确认计划
	if summary.Pending > 0 {
		actions = append(actions, fmt.Sprintf("📋 有%d条待确认的交易计划，请审阅后确认或忽略", summary.Pending))
	}

	// 已确认待执行
	if summary.Confirmed > 0 {
		actions = append(actions, fmt.Sprintf("⏳ 有%d条已确认计划等待执行反馈，请在券商成交后反馈", summary.Confirmed))
	}

	// 决策变更
	changeCount := len(changes)
	if changeCount > 0 {
		actions = append(actions, fmt.Sprintf("🔄 检测到%d个标的投资决策发生变化，请关注", changeCount))
	}

	// 无操作提示
	if len(actions) == 0 {
		actions = append(actions, "✅ 当前无需操作，系统运行正常")
	}

	return actions
}

// HandlePlanList GET /api/plan?date=YYYYMMDD
// 查询交易计划列表, 不传 date 时返回全部待处理计划
func (s *Service) HandlePlanList(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")

	var plans []store.TradePlan
	var err error
	if date != "" {
		plans, err = s.planRepo.GetPlansByDate(parseDateParam(date))
	} else {
		plans, err = s.planRepo.GetOpenPlans()
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

	plan, err := s.planRepo.GetPlanByID(req.ID)
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

	if err := s.planRepo.UpdatePlanStatus(plan.ID, status); err != nil {
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
