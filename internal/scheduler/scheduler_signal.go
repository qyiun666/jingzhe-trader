package scheduler

import (
	"fmt"

	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// runSignalWithFreshnessCheck 信号生成前检查数据新鲜度, 不新鲜则阻塞等待数据更新完成
// 场景: 15:10 数据更新失败 (Tushare 延迟), 15:30 信号生成时自动补拉
func (s *Scheduler) runSignalWithFreshnessCheck(date string) error {
	barRepo := store.NewBarRepo(s.db)
	maxDate, err := barRepo.GetMaxTradeDate()
	if err != nil {
		// 无法确认数据新鲜度时宁缺毋滥: 不用未知状态的数据做交易决策
		s.alert("🚨 惊蛰信号中止", fmt.Sprintf("查询数据新鲜度失败: %v, 今日信号生成中止, 请检查数据库", err))
		return fmt.Errorf("查询数据新鲜度失败, 中止信号生成: %w", err)
	}
	if maxDate < date {
		logger.L().Infow("信号任务: 检测到数据不新鲜, 触发阻塞式数据更新", "latest_data", maxDate, "expected", date)
		s.alert("📅 惊蛰数据更新", fmt.Sprintf(
			"信号生成前检测到数据不新鲜 (库内最新: %s, 期望: %s), 自动触发数据更新",
			maxDate, date))
		if err := s.svc.UpdateDataBlocking(); err != nil {
			// 硬失败: 陈旧数据生成的计划是错误决策的来源, 宁可今日无计划
			s.alert("🚨 惊蛰信号中止", fmt.Sprintf("数据更新失败: %v, 今日信号生成中止 (不用陈旧数据做决策)", err))
			return fmt.Errorf("前置数据更新失败, 中止信号生成: %w", err)
		}
		// 更新成功后再次校验: Tushare 延迟时更新"成功"也可能没有当日数据
		if maxDate2, err := barRepo.GetMaxTradeDate(); err != nil || maxDate2 < date {
			s.alert("🚨 惊蛰信号中止", fmt.Sprintf(
				"数据更新后仍无当日数据 (库内最新: %s, 期望: %s), 今日信号生成中止, 请检查数据源", maxDate2, date))
			return fmt.Errorf("数据更新后仍不新鲜 (最新: %s, 期望: %s), 中止信号生成", maxDate2, date)
		}
		logger.L().Info("信号任务: 前置数据更新完成")
	} else {
		logger.L().Infow("信号任务: 数据新鲜度检查通过", "latest_data", maxDate)
	}

	// 选股结果新鲜度检查: 选股器启用时, 检查当日选股任务是否已成功
	// 失败时不中止整个信号 (卖出/风控计划照常生成), 仅跳过选股池合并 (买入侧)
	if s.cfg.Screener.Enabled {
		screenRepo := s.svc.ScreenRepo()
		if screenRepo == nil {
			logger.L().Warnw("选股结果仓库未初始化, 跳过选股池合并", "date", date)
		} else {
			// 检查当日选股任务是否已成功 (避免竞态: screener 未完成时 signal 提前检查)
			if done, err := s.jobRepo.HasSucceeded(store.JobScreener, date); err != nil {
				logger.L().Warnw("查询选股任务状态失败, 跳过选股池合并", "date", date, "err", err)
			} else if !done {
				logger.L().Warnw("选股任务尚未成功, 跳过选股池合并", "date", date)
				s.alert("⚠️ 惊蛰选股池跳过", fmt.Sprintf("选股任务尚未成功, 信号生成跳过选股池合并 (卖出/风控计划照常)"))
			} else {
				latestScreenDate, err := screenRepo.GetLatestDate()
				if err != nil {
					logger.L().Warnw("查询选股结果新鲜度失败, 跳过选股池合并", "date", date, "err", err)
				} else if latestScreenDate != date {
					logger.L().Warnw("选股结果过期, 跳过选股池合并", "date", date, "latest", latestScreenDate)
					s.alert("⚠️ 惊蛰选股池跳过", fmt.Sprintf("选股结果过期 (最新: %s, 期望: %s), 跳过选股池合并", latestScreenDate, date))
				}
			}
		}
	}
	return s.runSignal(date)
}

// runSignal 15:30 EOD 信号生成 → 次日交易计划落库
// 增强通知: 无论有无计划都通知用户, 包含决策变更和状态汇总
func (s *Scheduler) runSignal(date string) error {
	plans, err := s.svc.GenerateTradePlans(date)
	if err != nil {
		return fmt.Errorf("生成交易计划失败: %w", err)
	}
	// 旧的 pending 计划过期, 再写入当日新计划
	if n, err := s.planRepo.ExpireOldPlans(date); err != nil {
		logger.L().Warnw("过期旧计划失败", "err", err)
	} else if n > 0 {
		logger.L().Infow("过期旧计划", "count", n)
	}
	if err := s.planRepo.ReplaceDayPlans(date, plans); err != nil {
		return fmt.Errorf("交易计划落库失败: %w", err)
	}
	logger.L().Infow("EOD交易计划生成完成", "date", date, "count", len(plans))

	// 增强通知: 无论有无计划都通知
	s.notifySignalResult(date, plans)
	return nil
}

// notifySignalResult 发送信号生成结果通知 (每次都通知, 包含决策变更检测)
func (s *Scheduler) notifySignalResult(date string, plans []*store.TradePlan) {
	var lines []string

	// 计划汇总
	buyCount := 0
	sellCount := 0
	urgentCount := 0
	for _, p := range plans {
		if p.Direction == "buy" {
			buyCount++
		} else {
			sellCount++
		}
		if p.Urgency == store.PlanUrgencyUrgent {
			urgentCount++
		}
	}

	if len(plans) == 0 {
		lines = append(lines, fmt.Sprintf("📅 %s 信号生成完成: 今日无交易计划", date))
		lines = append(lines, "策略未触发买卖信号, 继续持有当前仓位")
	} else {
		summary := fmt.Sprintf("📋 %s 信号生成完成: 共%d条计划 (买入%d/卖出%d", date, len(plans), buyCount, sellCount)
		if urgentCount > 0 {
			summary += fmt.Sprintf(", 紧急%d", urgentCount)
		}
		summary += ")"
		lines = append(lines, summary)
		// 列出前5条计划
		showCount := len(plans)
		if showCount > 5 {
			showCount = 5
		}
		for i := 0; i < showCount; i++ {
			p := plans[i]
			icon := "🟢"
			if p.Direction == "sell" {
				icon = "🔴"
			}
			if p.Urgency == store.PlanUrgencyUrgent {
				icon = "🚨"
			}
			lines = append(lines, fmt.Sprintf("%s %s %s %d股 @%.2f", icon, p.Name, p.Direction, p.Qty, p.RefPrice))
		}
		if len(plans) > 5 {
			lines = append(lines, fmt.Sprintf("... 还有%d条, 详见 /api/plan", len(plans)-5))
		}
	}

	// 决策变更检测
	if todayDebates, err := store.NewDebateRepo(s.db).GetByDate(date); err == nil && len(todayDebates) > 0 {
		if s.svc.DebateOrchestrator() != nil && s.svc.DebateOrchestrator().IsEnabled() {
			changes := s.svc.DebateOrchestrator().DetectDecisionChanges(todayDebates)
			if len(changes) > 0 {
				lines = append(lines, "")
				lines = append(lines, fmt.Sprintf("🔄 决策变更检测: %d个标的决策发生变化", len(changes)))
				showChanges := len(changes)
				if showChanges > 3 {
					showChanges = 3
				}
				for i := 0; i < showChanges; i++ {
					lines = append(lines, fmt.Sprintf("  - %s: %s", changes[i].Name, changes[i].Detail))
				}
				if len(changes) > 3 {
					lines = append(lines, fmt.Sprintf("  ... 还有%d项变更", len(changes)-3))
				}
			}
		}
	}

	// 待处理计划提醒
	if openPlans, err := s.planRepo.GetOpenPlans(); err == nil {
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
			lines = append(lines, "")
			if pending > 0 {
				lines = append(lines, fmt.Sprintf("⏸️ 待确认: %d条 (请审阅 /api/plan)", pending))
			}
			if confirmed > 0 {
				lines = append(lines, fmt.Sprintf("⏳ 待反馈: %d条 (已确认等待成交反馈)", confirmed))
			}
		}
	}

	// 发送通知
	msg := ""
	for i, l := range lines {
		if i > 0 {
			msg += "\n"
		}
		msg += l
	}
	s.alert("📋 惊蛰交易信号", msg)
}
