package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"jingzhe-trader/internal/agent"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/engine"
	"jingzhe-trader/internal/goal"
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

	// 风控管理器: 与回测/实盘同一套配置, 并按季度目标状态收紧 (只收紧不放松)
	riskCfg := s.cfg.Risk
	if adj, notes := s.goalAdjustedRisk(date); len(notes) > 0 {
		riskCfg = adj
		for _, n := range notes {
			logger.L().Infof("[计划生成] 目标风控调节: %s", n)
		}
	}
	rm := risk.NewRiskManager(riskCfg)
	rm.SetSizeLimits(risk.SizeLimits{
		MinTradeAmount: s.cfg.Trading.MinTradeAmount,
		MaxPositions:   s.cfg.Trading.MaxPositions,
		MinCommission:  s.cfg.Cost.MinCommission,
	})

	// 止损信号优先, 策略信号对同一股票的信号剔除 (与回测 Pipeline 共用同一套合并/检查/排序语义)
	stopSignals := rm.CheckStopLoss(positions, todayBars)
	stopCodes := engine.StopCodesOf(stopSignals)
	strategyName := s.SelectStrategy(date)
	stratSignals, err := s.runStrategy(date, strategyName, todayBars, positions, asset)
	if err != nil {
		return nil, err
	}
	merged := engine.MergeStrategySignals(date, stopSignals, stopCodes, stratSignals)

	// 智能体辩论增强 (LLM可用时对买入信号跑辩论; 回测中可通过同款 hook 验证)
	if s.debateOrchestrator != nil && s.debateOrchestrator.IsEnabled() {
		merged = s.debateOrchestrator.EnhanceSignals(date, merged, todayBars, positions, asset.TotalAsset, s.stockMap)
	}

	passed, rejections := engine.CheckAndSortSignals(date, rm, merged, positions, asset.TotalAsset, s.loadRiskStocks(merged), todayBars)

	// 升级告警: 止损信号被风控拦截 (如跌停无法卖出/持仓不足) 必须让用户知道, 不能静默丢失
	s.escalateStopLossRejections(date, rejections, stopCodes)

	return s.signalsToPlans(date, strategyName, passed, todayBars, stopCodes), nil
}

// escalateStopLossRejections 止损类信号被风控拦截时写告警并记录错误日志
// 场景: 连续跌停时止损单会被"跌停禁卖"拦截, 用户必须知道持仓仍暴露在风险中
func (s *Service) escalateStopLossRejections(date string, rejections []engine.RejectInfo, stopCodes map[string]bool) {
	for _, rej := range rejections {
		if !stopCodes[rej.TsCode] {
			continue // 只升级止损类拦截
		}
		logger.L().Errorf("[%s] 止损信号被风控拦截(无法执行): %s %s (%s)", date, rej.TsCode, rej.Reason, rej.Rule)
		if s.alertRepo != nil {
			_, err := s.alertRepo.Insert(&store.AgentAlert{
				TradeDate: date,
				JobName:   "signal",
				Level:     store.AlertLevelUrgent,
				Title:     "🚨 止损无法执行",
				Content:   fmt.Sprintf("%s: %s (%s). 止损计划被风控拦截, 持仓仍暴露, 请人工关注!", rej.TsCode, rej.Reason, rej.Rule),
			})
			if err != nil {
				logger.L().Warnw("止损拦截告警入库失败", "ts_code", rej.TsCode, "err", err)
			}
		}
	}
}

// (sellFirstSort 已删除: 排序语义统一由 engine.CheckAndSortSignals 提供, 避免两套实现漂移)

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
			// 查不到股票信息的标的一律按"未上市"处理, 被黑名单拦截, 不默认放行
			logger.L().Warnf("[风控] %s 无股票基本信息, 按黑名单拦截处理", sig.TsCode)
			stocks[sig.TsCode] = &model.Stock{TsCode: sig.TsCode, ListStatus: "P"}
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
	Date              string                 `json:"date"`                // 数据基准日期
	DataLastDate      string                 `json:"data_last_date"`      // 数据库最新行情日期
	DataFresh         bool                   `json:"data_fresh"`          // 数据是否新鲜
	OpenPlans         []store.TradePlan      `json:"open_plans"`          // 待处理的交易计划
	Portfolio         *PortfolioJSON         `json:"portfolio"`           // 持仓诊断
	Market            *MarketSnapshotJSON    `json:"market"`              // 市场概况
	Jobs              map[string]string      `json:"jobs"`                // 各任务最近成功时间
	Warnings          []string               `json:"warnings"`            // 数据/任务异常提示
	Debates           []store.DebateResult   `json:"debates"`             // 当日辩论结果
	ScreenResults     []store.ScreenResult   `json:"screen_results"`      // 最新选股结果
	ActionNeeded      []string               `json:"action_needed"`       // 需要用户操作的提示
	DecisionChanges   []agent.DecisionChange `json:"decision_changes"`    // 决策变更记录
	PlanStatusSummary PlanStatusSummary      `json:"plan_status_summary"` // 交易计划状态汇总
	TaskCompleted     map[string]bool        `json:"task_completed"`      // 当日各任务是否已完成
	Goal              *goal.Status           `json:"goal,omitempty"`           // 季度目标状态 (目标跟踪启用时)
}

// PlanStatusSummary 交易计划状态汇总
type PlanStatusSummary struct {
	Pending   int `json:"pending"`   // 待确认
	Confirmed int `json:"confirmed"` // 已确认待执行
	Executed  int `json:"executed"`  // 已执行
	Expired   int `json:"expired"`   // 已过期
	Total     int `json:"total"`     // 总计
}

// HandleAgentBrief GET /api/agent/brief
// 一次返回 Agent 决策所需全部上下文
func (s *Service) HandleAgentBrief(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.BuildAgentBrief())
}

// BuildAgentBrief builds the full Agent context without writing an HTTP response.
func (s *Service) BuildAgentBrief() *AgentBrief {
	brief := &AgentBrief{Jobs: map[string]string{}, Warnings: []string{}}

	// 数据新鲜度
	lastDate, err := s.barRepo.GetMaxTradeDate()
	if err != nil || lastDate == "" {
		brief.Warnings = append(brief.Warnings, "数据库无行情数据, 请先执行数据更新")
		return brief
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

	// 季度目标状态 (Agent 决策的核心约束之一)
	if s.goalTracker != nil {
		if st, gerr := s.GoalStatus(lastDate); gerr == nil {
			brief.Goal = st
			if st.Mode != goal.ModeNormal {
				brief.Warnings = append(brief.Warnings, fmt.Sprintf("目标风控模式: %s — %s", st.ModeLabel, strings.Join(st.Notes, "; ")))
			}
		}
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

	return brief
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
	writeJSON(w, http.StatusOK, s.BuildPlans(date))
}

// BuildPlans returns trade plans for a date, or all open plans if date is empty.
func (s *Service) BuildPlans(date string) []store.TradePlan {
	var plans []store.TradePlan
	var err error
	if date != "" {
		plans, err = s.planRepo.GetPlansByDate(parseDateParam(date))
	} else {
		plans, err = s.planRepo.GetOpenPlans()
	}
	if err != nil {
		return []store.TradePlan{}
	}
	return plans
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
	plan, err := s.ConfirmPlan(req.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// ConfirmPlan confirms a trade plan by ID and returns the updated plan.
func (s *Service) ConfirmPlan(id int64) (*store.TradePlan, error) {
	plan, err := s.planRepo.GetPlanByID(id)
	if err != nil {
		return nil, err
	}
	if plan.Status != store.PlanStatusPending {
		return nil, fmt.Errorf("计划状态为 %s, 无法确认", plan.Status)
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
			return nil, fmt.Errorf("QMT下单失败: %w", err)
		}
		if nerr := notifier.SendText(fmt.Sprintf("✅ 惊蛰下单成功\n%s %s %d股 @%.2f (%s)",
			plan.TsCode, plan.Direction, plan.Qty, plan.RefPrice, plan.Reason)); nerr != nil {
			logger.L().Warnw("飞书通知发送失败", "err", nerr)
		}
		status = store.PlanStatusExecuted
	}

	if err := s.planRepo.UpdatePlanStatus(plan.ID, status); err != nil {
		return nil, err
	}
	plan.Status = status
	return plan, nil
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
