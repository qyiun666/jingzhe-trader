package api

// 盘前总结 (09:00 邮件) 数据构建
// 数据日期 = 上一交易日 (盘前当日行情尚未产生, 用上一交易日收盘数据)

import (
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/store"
)

// PremarketSummary 盘前总结数据
type PremarketSummary struct {
	Date        string              `json:"date"`         // 总结日期 (交易日 YYYYMMDD)
	DataDate    string              `json:"data_date"`    // 数据日期 (上一交易日)
	Market      *MarketSnapshotJSON `json:"market"`       // 昨日市场概况
	Portfolio   *PortfolioJSON      `json:"portfolio"`    // 当前持仓诊断
	OpenPlans   []store.TradePlan   `json:"open_plans"`   // 今日待执行计划 (前一日 18:00 生成)
	Goal        *goal.Status        `json:"goal"`         // 季度目标状态
	AlertsCount int                 `json:"alerts_count"` // 数据日期告警数
	Warnings    []string            `json:"warnings"`     // 风险提示
}

// BuildPremarketSummary 构建盘前总结
func (s *Service) BuildPremarketSummary() *PremarketSummary {
	today := time.Now().Format("20060102")
	sum := &PremarketSummary{Date: today, Warnings: []string{}}

	// 数据日期: 上一交易日
	preDate, err := s.calRepo.GetPreTradeDate(today)
	if err != nil || preDate == "" {
		sum.Warnings = append(sum.Warnings, "交易日历缺失, 无法确定上一交易日")
		return sum
	}
	sum.DataDate = preDate

	// 数据新鲜度检查: 行情数据晚于上一交易日时计划参考价可能过期
	if last, lerr := s.barRepo.GetMaxTradeDate(); lerr == nil && last < preDate {
		sum.Warnings = append(sum.Warnings, fmt.Sprintf("行情数据最新到 %s, 晚于上一交易日 %s, 计划参考价可能过期", last, preDate))
	}

	// 昨日市场概况
	if market, merr := s.RunMarket(preDate); merr == nil {
		sum.Market = market
	}

	// 当前持仓诊断
	if portfolio, perr := s.RunPositions(preDate); perr == nil {
		sum.Portfolio = portfolio
	}

	// 今日待执行计划 (前一日 18:00 信号生成)
	if plans, perr := s.planRepo.GetOpenPlans(); perr == nil {
		sum.OpenPlans = plans
	}

	// 季度目标状态 (风险模式非正常时提示)
	if s.goalTracker != nil {
		if st, gerr := s.GoalStatus(preDate); gerr == nil {
			sum.Goal = st
			if st.Mode != goal.ModeNormal {
				sum.Warnings = append(sum.Warnings, fmt.Sprintf("目标风控模式: %s — %s", st.ModeLabel, strings.Join(st.Notes, "; ")))
			}
		}
	}

	// 数据日期告警数 (日报邮件会展示明细, 盘前仅提示数量)
	if alerts, aerr := store.NewAlertRepo(s.db).GetByDate(preDate); aerr == nil {
		sum.AlertsCount = len(alerts)
	}

	return sum
}
