package api

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/engine"
	"jingzhe-trader/internal/model"
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
			barMap := barsToMap(allBars)
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
	todayBars := barsToMap(allBars)

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
		enhanced, persistFailures := s.debateOrchestrator.EnhanceSignals(date, merged, todayBars, positions, asset.TotalAsset, s.stockMap)
		merged = enhanced
		s.escalateDebatePersistFailures(date, persistFailures)
	}

	passed, rejections := engine.CheckAndSortSignals(date, rm, merged, positions, asset.TotalAsset, s.loadRiskStocks(merged), todayBars)

	// 升级告警: 止损信号被风控拦截 (如跌停无法卖出/持仓不足) 必须让用户知道, 不能静默丢失
	s.escalateStopLossRejections(date, rejections, stopCodes)

	return s.signalsToPlans(date, strategyName, passed, todayBars, stopCodes), nil
}

// escalateDebatePersistFailures 辩论结论落库失败时汇总一条告警
// 信号本身已按结论增强完毕, 但 agent_debate 缺行会让 ReviewDebates 拿不到样本,
// 反思闭环 (复盘命中率回填后续辩论) 就静默失效了 —— 花了 LLM 调用却没留下可验证的记录
func (s *Service) escalateDebatePersistFailures(date string, failures []string) {
	if len(failures) == 0 || s.alertRepo == nil {
		return
	}
	logger.L().Errorf("[%s] %d 条辩论结论落库失败: %s", date, len(failures), strings.Join(failures, "; "))
	if _, err := s.alertRepo.Insert(&store.AgentAlert{
		TradeDate: date,
		JobName:   "signal",
		Level:     store.AlertLevelWarning,
		Title:     "⚠️ 辩论结论未入库",
		Content: fmt.Sprintf("%d 条辩论结论落库失败, 反思闭环当日无样本可回填:\n- %s",
			len(failures), strings.Join(failures, "\n- ")),
	}); err != nil {
		logger.L().Warnw("辩论落库失败告警入库失败", "err", err)
	}
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
