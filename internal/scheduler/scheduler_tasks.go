package scheduler

import (
	"fmt"
	"path/filepath"
	"strings"

	"jingzhe-trader/internal/agent"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/report"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// ==================== 具体任务实现 ====================

// runDataUpdate 15:10 数据更新 (进程内调用 dataloader)
func (s *Scheduler) runDataUpdate(date string) error {
	if err := s.svc.UpdateData(); err != nil {
		return fmt.Errorf("数据更新失败: %w", err)
	}
	// 数据新鲜后记录实盘账户快照 (收益曲线; 失败不阻断主任务)
	// 与信号任务同策略: 快照成功才重评风险模式, 避免用未收盘的旧市值误切模式 (18:00 信号补记会再评)
	if err := s.svc.RecordLiveSnapshot(date); err != nil {
		logger.L().Warnw("实盘账户快照记录失败", "date", date, "err", err)
	} else {
		s.checkGoalMode(date)
	}
	return nil
}

// checkGoalMode 评估季度目标状态, 风险模式变化时告警并记录
// 可能同日多次被调用 (15:10 数据更新 / 18:00 信号补记快照), goalMu 保证读-改-写原子, 不会重复告警
func (s *Scheduler) checkGoalMode(date string) {
	s.goalMu.Lock()
	defer s.goalMu.Unlock()
	st, err := s.svc.GoalStatus(date)
	if err != nil || st == nil {
		return // 未启用目标跟踪
	}
	portRepo := store.NewPortfolioRepo(s.db)
	lastMode, _ := portRepo.GetMeta("goal_risk_mode")
	if st.Mode == lastMode {
		return
	}
	portRepo.SetMeta("goal_risk_mode", st.Mode)
	logger.L().Infow("季度目标风险模式切换", "date", date, "from", lastMode, "to", st.Mode,
		"return", st.ReturnPct, "progress", st.Progress, "drawdown", st.DrawdownPct)
	s.alert("🎯 惊蛰目标跟踪", fmt.Sprintf(
		"%s 风险模式: %s → %s\n季度收益: %.2f%% (目标 %.1f%%, 进度 %.0f%%)\n回撤: %.2f%% (预算 %.1f%%, 消耗 %.0f%%)\n%s",
		st.Quarter, goal.ModeLabel(lastMode), st.ModeLabel,
		st.ReturnPct*100, st.TargetPct*100, st.Progress*100,
		st.DrawdownPct*100, st.BudgetPct*100, st.BudgetConsumed*100,
		strings.Join(st.Notes, "; ")))
}

// runScreener 15:15 自动选股 (数据更新后, 信号生成前)
// 全市场筛选候选股票 → 同步历史K线 → 结果落库 → 通知
func (s *Scheduler) runScreener(date string) error {
	if s.svc.Screener() == nil {
		return nil
	}
	results, err := s.svc.Screener().Screen(date)
	if err != nil {
		return fmt.Errorf("选股失败: %w", err)
	}

	// 通知
	var lines []string
	if len(results) == 0 {
		lines = append(lines, fmt.Sprintf("📅 %s 全市场选股完成: 无符合条件的候选股票", date))
	} else {
		lines = append(lines, fmt.Sprintf("🔍 %s 全市场选股完成: 筛选出 %d 只候选股票", date, len(results)))
		lines = append(lines, "前5名:")
		for i, c := range results {
			if i >= 5 {
				break
			}
			lines = append(lines, fmt.Sprintf("  #%d %s %s  收盘%.2f 涨跌%.1f%% 换手%.1f%% PE=%.0f 评分%.1f",
				i+1, c.TsCode, c.Name, c.Close, c.PctChg, c.TurnoverRate, c.PE_TTM, c.Score))
		}
		lines = append(lines, "候选股票已自动加入策略股票池, 15:30 信号生成时将一并扫描")
	}
	s.alert("🔍 惊蛰自动选股", strings.Join(lines, "\n"))

	logger.L().Infow("选股任务完成", "date", date, "candidates", len(results))
	return nil
}

// runDebateReview 辩论决策复盘: 回填满窗口期(5自然日)辩论结论的实际收益
// 反思闭环数据源: 复盘结果通过 DebateContext.ReviewSummary 注入后续辩论
func (s *Scheduler) runDebateReview(date string) error {
	reviewed, err := agent.ReviewDebates(
		store.NewDebateRepo(s.db),
		store.NewDebateReviewRepo(s.db),
		store.NewBarRepo(s.db),
		date,
	)
	if err != nil {
		return fmt.Errorf("辩论复盘失败: %w", err)
	}
	if len(reviewed) == 0 {
		return nil
	}
	correct := 0
	for _, r := range reviewed {
		if r.Correct == 1 {
			correct++
		}
	}
	logger.L().Infow("辩论复盘回填完成", "date", date, "reviewed", len(reviewed), "correct", correct)
	s.alert("🧠 惊蛰辩论复盘", fmt.Sprintf(
		"%s 回填 %d 条辩论结论验证: 命中 %d/%d (%.0f%%)\n复盘数据将注入后续辩论上下文 (反思闭环)",
		date, len(reviewed), correct, len(reviewed), float64(correct)/float64(len(reviewed))*100))
	return nil
}

// runReconcile 15:35 对账 (仅 QMT 实盘): 本地记录 vs 券商
// 复用组合根注入的长生命周期 broker (新建 QMTBridge 会丢失 OnTrade 成交回调,
// 导致 PollTrades 触发时回调列表为空, 真实成交静默丢弃)
func (s *Scheduler) runReconcile(date string) error {
	brk := s.svc.Broker()
	result, err := report.Reconcile(brk, store.NewPortfolioRepo(s.db), store.NewActionRepo(s.db), date)
	if err != nil {
		return fmt.Errorf("对账执行失败: %w", err)
	}
	if !result.IsBalanced {
		s.alert("⚠️ 惊蛰对账差异", report.GenerateReconcileReport(result))
	}
	// 成交回报轮询: 把券商端真实成交落库 (OnTrade 回调 → action_log, kind=trade)
	if err := s.svc.PollBrokerTrades(); err != nil {
		logger.L().Warnw("成交回报轮询失败", "date", date, "err", err)
	}
	return nil
}

// runPremarket 09:00 盘前总结邮件 (昨日复盘 + 当前持仓 + 今日计划 + 目标状态)
// 数据来自上一交易日收盘: 盘前当日行情尚未产生
func (s *Scheduler) runPremarket(date string) error {
	sum := s.svc.BuildPremarketSummary()
	if !s.mailer.Enabled() {
		s.warnMailDisabled("盘前总结")
		logger.L().Info("邮件未配置, 盘前总结仅记录")
		return nil
	}
	title := fmt.Sprintf("📊 惊蛰盘前总结 %s", sum.DataDate)
	if err := s.mailer.SendHTML(title, buildPremarketHTML(sum)); err != nil {
		logger.L().Warnw("盘前总结邮件发送失败", "err", err)
		return fmt.Errorf("盘前总结邮件发送失败: %w", err)
	}
	logger.L().Infow("盘前总结邮件已发送", "data_date", sum.DataDate, "plans", len(sum.OpenPlans))
	return nil
}

// runReport 18:00 当天总结 + 日报邮件 (含当日告警汇总)
// 增强通知: 日报推送后追加操作提醒 (落库, Agent 可读)
func (s *Scheduler) runReport(date string) error {
	daily, err := s.svc.RunDaily(date, s.svc.SelectStrategy(date))
	if err != nil {
		return fmt.Errorf("生成日报失败: %w", err)
	}
	// 当日告警汇总 (任务失败/数据更新/对账等过程通知统一进日报, 不单独打扰)
	var alerts []store.AgentAlert
	if list, aerr := store.NewAlertRepo(s.db).GetByDate(date); aerr == nil {
		alerts = list
	}
	// 追加操作提醒: 检查是否有待处理计划 (先落库, 与邮件推送解耦; Agent 可离线读取)
	if openPlans, err := s.planRepo.GetOpenPlans(); err == nil && len(openPlans) > 0 {
		pending := 0
		confirmed := 0
		for _, p := range openPlans {
			if p.Status == store.PlanStatusPending {
				pending++
			}
			if p.Status == store.PlanStatusConfirmed {
				confirmed++
			}
		}
		if pending > 0 || confirmed > 0 {
			reminder := fmt.Sprintf("📊 日报已生成, 请查看:\n")
			if pending > 0 {
				reminder += fmt.Sprintf("⏸️ %d条计划待确认 (POST /api/plan/confirm)\n", pending)
			}
			if confirmed > 0 {
				reminder += fmt.Sprintf("⏳ %d条计划待执行反馈 (POST /api/trade/confirm)\n", confirmed)
			}
			reminder += "完整数据: GET /api/agent/brief | 变更检测: GET /api/agent/changes"
			s.alert("📌 惊蛰操作提醒", reminder)
		}
	}
	if !s.mailer.Enabled() {
		s.warnMailDisabled("日报推送")
		logger.L().Info("邮件未配置, 日报仅落库不推送")
		return nil
	}
	if err := s.mailer.SendHTML("📊 惊蛰日报 "+date, buildDailyMailHTML(daily, alerts)); err != nil {
		// 返回错误让 job_run 记为 failed 并触发冷却重试, 与 runPremarket 一致;
		// 否则发送失败也会全绿 (无声故障)
		return fmt.Errorf("日报邮件发送失败: %w", err)
	}
	return nil
}

// runSettleT1 每日 09:25 T+1 持仓结转 (昨日买入转为可卖)
func (s *Scheduler) runSettleT1(date string) error {
	if err := s.svc.SettleT1(date); err != nil {
		s.alert("⚠️ 惊蛰T+1结转失败", fmt.Sprintf("%v, 今日可卖量可能不准确", err))
		return fmt.Errorf("T+1持仓结转失败: %w", err)
	}
	return nil
}

// runIntradayMonitor 盘中止损监控: 实时价 → 止损检查 → 紧急卖出计划 + 告警
func (s *Scheduler) runIntradayMonitor(date string) error {
	portRepo := store.NewPortfolioRepo(s.db)
	positions, err := portRepo.GetAllPositions()
	if err != nil {
		return fmt.Errorf("查询持仓失败: %w", err)
	}
	if len(positions) == 0 {
		return nil
	}

	codes := make([]string, 0, len(positions))
	for _, p := range positions {
		codes = append(codes, p.TsCode)
	}
	prices, err := s.quoteSrc.GetRealtimePrices(codes)
	if err != nil {
		return fmt.Errorf("拉取实时行情失败: %w", err)
	}

	// 已有未处理紧急计划的股票不重复告警
	existing := map[string]bool{}
	if plans, err := s.planRepo.GetPlansByDate(date); err == nil {
		for _, p := range plans {
			if p.Urgency == store.PlanUrgencyUrgent && p.Status == store.PlanStatusPending {
				existing[p.TsCode] = true
			}
		}
	}

	sl := risk.NewStopLossManager(s.cfg.Risk.StopLossPct, s.cfg.Risk.TakeProfitPct)
	if s.cfg.Risk.TrailingStopPct > 0 {
		sl.SetTrailingStop(s.cfg.Risk.TrailingStopPct)
	}
	for _, p := range positions {
		price, ok := prices[p.TsCode]
		if !ok || existing[p.TsCode] {
			continue
		}
		pos := &model.Position{
			TsCode:       p.TsCode,
			TotalQty:     p.TotalQty,
			AvailableQty: p.AvailableQty,
			CostPrice:    p.CostPrice,
			HighPrice:    p.HighPrice,
		}
		// 盘中刷新持仓期间最高价 (移动止盈基准)
		if price > p.HighPrice {
			if err := portRepo.UpdateHighPrice(p.TsCode, price); err != nil {
				logger.L().Warnw("更新持仓最高价失败", "ts_code", p.TsCode, "err", err)
			}
		}
		triggered, reason := sl.CheckSingle(pos, price)
		if !triggered {
			continue
		}
		// 数量以可卖量为准并钳制到总持仓, 防止计划卖出量超过实际持仓导致拒单
		qty := p.AvailableQty
		if qty <= 0 {
			qty = p.TotalQty // T+1 不可卖也先生成计划提示
		}
		if qty > p.TotalQty {
			qty = p.TotalQty
		}
		plan := &store.TradePlan{
			TradeDate: date,
			TsCode:    p.TsCode,
			Direction: "sell",
			Qty:       qty,
			RefPrice:  price,
			Reason:    "盘中" + reason,
			Strategy:  "stop_loss",
			Urgency:   store.PlanUrgencyUrgent,
		}
		if _, err := s.planRepo.InsertPlan(plan); err != nil {
			logger.L().Errorw("紧急计划落库失败", "ts_code", p.TsCode, "err", err)
			continue
		}
		msg := fmt.Sprintf("%s 现价 %.2f 触发: %s\n已生成紧急卖出计划(%d股), 请尽快确认", p.TsCode, price, reason, qty)
		// 跌停时止损单可能无法成交, 必须显式提示 (连续跌停场景用户容易无感知)
		if limit, err := store.NewLimitRepo(s.db).GetByCodeAndDate(p.TsCode, date); err == nil && limit != nil &&
			limit.DownLimit > 0 && price <= limit.DownLimit {
			msg += fmt.Sprintf("\n⚠️ 现价已触及跌停价 %.2f, 卖出单可能无法成交, 请立即人工处理!", limit.DownLimit)
		}
		s.alert("🚨 惊蛰盘中止损告警", msg)
		// 止损需要立即操作, 即时邮件 (需操作的通知单独发; 其余告警汇总进日报)
		if !s.mailer.Enabled() {
			s.warnMailDisabled("止损即时告警")
		} else if serr := s.mailer.Send("🚨 惊蛰盘中止损告警", msg); serr != nil {
			logger.L().Warnw("止损告警邮件发送失败", "err", serr)
		}
	}
	return nil
}

// runRetention 16:30 数据保留清理 + SQLite 瘦身
func (s *Scheduler) runRetention(date string, fullClean bool) error {
	rc := s.cfg.Retention
	logDir := ""
	if s.cfg.Log.FilePath != "" {
		logDir = filepath.Dir(s.cfg.Log.FilePath)
	}
	if err := store.RunRetention(s.db, store.RetentionPolicy{
		BarYears:     rc.BarYears,
		NewsDays:     rc.NewsDays,
		PlanDays:     rc.PlanDays,
		BacktestRuns: rc.BacktestRuns,
		LogDays:      rc.LogDays,
		ReportFiles:  rc.ReportFiles,
		LogDir:       logDir,
		ReportDir:    "reports",
	}, fullClean); err != nil {
		logger.L().Warnw("数据保留清理失败", "err", err)
	}

	// 清理不在活跃股票池中的陈旧数据 (选股结果+持仓+watchlist 之外的股票)
	if fullClean {
		keepCodes := store.GetActiveStockCodes(s.db, s.cfg.Dataloader.Watchlist, s.cfg.UniverseCodes())
		if err := store.CleanStaleStocks(s.db, keepCodes); err != nil {
			logger.L().Warnw("陈旧股票数据清理失败", "err", err)
		}
	}

	return nil
}
