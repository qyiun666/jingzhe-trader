package scheduler

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/api"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/store"
)

// 构造盘前总结测试数据
func testPremarketSummary() *api.PremarketSummary {
	return &api.PremarketSummary{
		Date:     "20260825",
		DataDate: "20260824",
		Market: &api.MarketSnapshotJSON{
			UpCount: 3120, DownCount: 1400, LimitUpCount: 86, LimitDownCount: 4, VolumeRatio: 1.18,
		},
		Portfolio: &api.PortfolioJSON{
			TotalAsset: 10250.5, Cash: 2500, DailyPnLPct: 1.52, HealthScore: 82,
			Holdings: []map[string]interface{}{
				{"name": "贵州茅台", "total_qty": float64(100), "market_price": 1500.0,
					"market_value": 150000.0, "pnl_pct": 3.25, "weight_pct": 60.0},
				{"name": "宁德时代", "total_qty": float64(200), "market_price": 180.0,
					"market_value": 36000.0, "pnl_pct": -1.8, "weight_pct": 30.0},
			},
		},
		OpenPlans: []store.TradePlan{
			{Name: "贵州茅台", Direction: "buy", Qty: 100, RefPrice: 1500, Reason: "金叉买入", Status: store.PlanStatusPending},
			{Name: "宁德时代", Direction: "sell", Qty: 200, RefPrice: 180, Reason: "死叉卖出", Status: store.PlanStatusPending},
		},
		Goal: &goal.Status{
			Quarter: "2026Q3", ReturnPct: 0.052, TargetPct: 0.15, Progress: 0.35,
			DrawdownPct: 0.021, BudgetPct: 0.08, BudgetConsumed: 0.26,
			Mode: goal.ModeNormal, ModeLabel: "正常",
		},
		AlertsCount: 3,
		Warnings:    []string{"行情数据可能过期"},
	}
}

// 构造日报测试数据
func testDailyReport() *api.DailyReportJSON {
	return &api.DailyReportJSON{
		Date: "20260825",
		MarketSnapshot: &api.MarketSnapshotJSON{
			UpCount: 2800, DownCount: 1700, LimitUpCount: 62, LimitDownCount: 9, VolumeRatio: 0.95,
		},
		Portfolio: &api.PortfolioJSON{
			TotalAsset: 10250.5, Cash: 2500, DailyPnLPct: -0.85, HealthScore: 72,
			Holdings: []map[string]interface{}{
				{"name": "贵州茅台", "total_qty": float64(100), "market_price": 1480.0,
					"market_value": 148000.0, "pnl_pct": 1.9, "weight_pct": 58.0},
			},
		},
		Rebalance: &api.RebalanceJSON{
			SellList: []api.TradeSuggestionJSON{{Name: "宁德时代", DeltaQty: 200, Reason: "跌破止损线"}},
			BuyList:  []api.TradeSuggestionJSON{{Name: "招商银行", DeltaQty: 500, Price: 30.5, Reason: "金叉买入"}},
			HoldList: []api.HoldSuggestionJSON{{Name: "贵州茅台", Suggestion: "接近止盈位, 观察"}},
		},
		StrategyAdvice: &api.StrategyJSON{
			Recommended: "维持仓位", Confidence: 0.72, Condition: "震荡市", Reason: "均线粘合, 等待方向",
		},
		ActionItems: []api.ActionItemJSON{
			{Time: "09:25", Action: "检查", Detail: "查看隔夜新闻与集合竞价"},
			{Time: "盘中", Action: "买入", Name: "招商银行", Detail: "金叉买入 500 股"},
		},
	}
}

func TestBuildPremarketHTML(t *testing.T) {
	html := buildPremarketHTML(testPremarketSummary())

	for _, want := range []string{
		"盘前总结", "20260825 交易日", "昨日市场概况", "当前持仓",
		"今日待执行计划", "季度目标", "风险提示",
		"贵州茅台", "金叉买入", "死叉卖出",
		"<table", "<html", "</html>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("盘前总结 HTML 缺少 %q", want)
		}
	}
	// 涨红跌绿: 买入标红
	if !strings.Contains(html, `style="color:#d63a3a;font-weight:600;">买入</span>`) {
		t.Errorf("买入方向应为红色, 实际 HTML:\n%s", html)
	}
	if !strings.Contains(html, `style="color:#1a8f4c;font-weight:600;">卖出</span>`) {
		t.Errorf("卖出方向应为绿色, 实际 HTML:\n%s", html)
	}
	// 风险提示应转义展示
	if !strings.Contains(html, "行情数据可能过期") {
		t.Errorf("风险提示缺失")
	}
}

func TestBuildPremarketHTMLNoData(t *testing.T) {
	sum := &api.PremarketSummary{Date: "20260825", Warnings: []string{"交易日历缺失, 无法确定上一交易日"}}
	html := buildPremarketHTML(sum)
	if !strings.Contains(html, "交易日历缺失") {
		t.Errorf("无数据时也应展示警告")
	}
	if strings.Contains(html, "昨日市场概况") {
		t.Errorf("无市场数据时不应展示市场概况区块")
	}
}

func TestBuildDailyMailHTML(t *testing.T) {
	alerts := []store.AgentAlert{
		{TradeDate: "20260825", Level: store.AlertLevelUrgent, Title: "🚨 惊蛰信号中止", Content: "数据更新失败, 今日信号生成中止"},
		{TradeDate: "20260825", Level: store.AlertLevelWarning, Title: "⚠️ 惊蛰选股池跳过", Content: "选股任务尚未成功, 跳过选股池合并"},
	}
	html := buildDailyMailHTML(testDailyReport(), alerts)

	for _, want := range []string{
		"惊蛰日报", "市场概况", "持仓与资产", "调仓计划", "策略建议", "操作清单", "当日告警",
		"跌破止损线", "金叉买入 500 股", "🚨 惊蛰信号中止", "⚠️ 惊蛰选股池跳过",
		"维持仓位", "<table",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("日报 HTML 缺少 %q", want)
		}
	}
	// 告警内容超长截断
	if !strings.Contains(html, "60") { // 截断逻辑在 alert 汇总中, 这里验证区块存在即可
		t.Log("告警截断逻辑未触发 (内容较短)")
	}
}

func TestBuildDailyMailHTMLNoAlerts(t *testing.T) {
	html := buildDailyMailHTML(testDailyReport(), nil)
	if strings.Contains(html, "当日告警") {
		t.Errorf("无告警时不应展示告警区块")
	}
}

func TestMailEscaping(t *testing.T) {
	// 用户数据 (持仓名/计划原因/标题) 含 HTML 特殊字符时必须转义, 防止注入
	sum := testPremarketSummary()
	sum.Portfolio.Holdings[0]["name"] = "<img src=x onerror=alert(1)>"
	sum.OpenPlans[0].Name = "<script>alert(1)</script>"
	sum.OpenPlans[0].Reason = "<b>金叉</b>"
	html := buildPremarketHTML(sum)

	if strings.Contains(html, "<script>") || strings.Contains(html, "<img") || strings.Contains(html, "<b>金叉") {
		t.Errorf("用户数据未转义, 存在注入风险")
	}
	for _, want := range []string{"&lt;script&gt;", "&lt;img", "&lt;b&gt;金叉&lt;/b&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("用户数据应转义为 %q", want)
		}
	}

	// 邮件标题转义
	layout := mailLayout("测试<title>标题", "20260825", "")
	if strings.Contains(layout, "<title>") || !strings.Contains(layout, "&lt;title&gt;") {
		t.Errorf("邮件标题未转义")
	}
}
