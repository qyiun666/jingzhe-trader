package scheduler

// 邮件 HTML 模板与构建器
// 内联样式保证主流邮件客户端 (QQ/163/Gmail) 渲染一致; A股惯例: 涨红跌绿

import (
	"fmt"
	"html"
	"strings"

	"jingzhe-trader/internal/api"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/store"
)

// ==================== 通用模板 ====================

// mailLayout 公共布局: 头部标题条 + 区块内容 + 页脚
func mailLayout(title, date string, sections ...string) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"></head><body style="margin:0;padding:16px;background:#f2f4f7;font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;">`)
	b.WriteString(`<div style="max-width:680px;margin:0 auto;background:#ffffff;border:1px solid #e2e6ec;border-radius:8px;overflow:hidden;">`)
	fmt.Fprintf(&b, `<div style="background:#1a3c6e;color:#ffffff;padding:16px 24px;"><div style="font-size:18px;font-weight:600;">%s</div><div style="font-size:12px;opacity:0.85;margin-top:4px;">%s</div></div>`,
		html.EscapeString(title), html.EscapeString(date))
	b.WriteString(`<div style="padding:20px 24px;">`)
	for _, sec := range sections {
		if sec != "" {
			b.WriteString(sec)
		}
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div style="padding:12px 24px;background:#fafbfc;border-top:1px solid #e2e6ec;font-size:11px;color:#98a2b3;">惊蛰量化交易系统 · 自动生成 · 邮件仅供参考, 交易决策以系统计划为准</div>`)
	b.WriteString(`</div></body></html>`)
	return b.String()
}

// mailSection 区块: 左侧色条标题 + 内容
func mailSection(title, content string) string {
	if content == "" {
		return ""
	}
	return fmt.Sprintf(`<div style="margin:0 0 18px 0;"><h3 style="margin:0 0 10px 0;padding:0 0 0 8px;border-left:3px solid #1a3c6e;font-size:14px;color:#1a3c6e;">%s</h3>%s</div>`,
		html.EscapeString(title), content)
}

// mailTable 数据表 (header 自动转义; cell 由调用方传入已排版 HTML)
func mailTable(headers []string, rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<table style="width:100%;border-collapse:collapse;font-size:13px;">`)
	b.WriteString(`<tr>`)
	for _, h := range headers {
		fmt.Fprintf(&b, `<th style="padding:7px 8px;background:#f0f4fa;color:#1a3c6e;text-align:left;border-bottom:2px solid #d7dfeb;white-space:nowrap;">%s</th>`, html.EscapeString(h))
	}
	b.WriteString(`</tr>`)
	for _, row := range rows {
		b.WriteString(`<tr>`)
		for _, cell := range row {
			fmt.Fprintf(&b, `<td style="padding:7px 8px;border-bottom:1px solid #eef1f5;">%s</td>`, cell)
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

// mailKV 键值行 (区块内统计)
func mailKV(pairs ...[2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&b, `<div style="display:inline-block;margin:0 18px 6px 0;font-size:13px;"><span style="color:#98a2b3;">%s</span> <b>%s</b></div>`, html.EscapeString(p[0]), p[1])
	}
	return b.String()
}

// pnlColor 涨红跌绿 (v 为百分比数值, 如 5.0 表示 5%)
func pnlColor(v float64) string {
	switch {
	case v > 0:
		return "#d63a3a"
	case v < 0:
		return "#1a8f4c"
	default:
		return "#555555"
	}
}

// pnlSpan 涨跌值着色
func pnlSpan(v float64) string {
	return fmt.Sprintf(`<span style="color:%s;font-weight:600;">%+.2f%%</span>`, pnlColor(v), v)
}

// esc 文本转义 (防止持仓名等用户数据注入 HTML)
func esc(s string) string { return html.EscapeString(s) }

// dirLabel 买卖方向标签 (A股惯例: 买入红/卖出绿)
func dirLabel(dir string) string {
	if dir == "buy" {
		return `<span style="color:#d63a3a;font-weight:600;">买入</span>`
	}
	return `<span style="color:#1a8f4c;font-weight:600;">卖出</span>`
}

// ==================== 盘前总结 ====================

// buildPremarketHTML 09:00 盘前总结邮件
func buildPremarketHTML(sum *api.PremarketSummary) string {
	var sections []string
	sections = append(sections, marketSection(sum.Market, "昨日市场概况"))
	sections = append(sections, portfolioSection(sum.Portfolio, "当前持仓"))
	sections = append(sections, planSection(sum.OpenPlans, "今日待执行计划"))
	if sum.Goal != nil {
		sections = append(sections, goalSection(sum.Goal))
	}
	if len(sum.Warnings) > 0 {
		var lines []string
		for _, w := range sum.Warnings {
			lines = append(lines, fmt.Sprintf(`<div style="color:#d63a3a;font-size:13px;margin:2px 0;">⚠️ %s</div>`, esc(w)))
		}
		sections = append(sections, mailSection("风险提示", strings.Join(lines, "")))
	}
	return mailLayout("盘前总结", fmt.Sprintf("%s 交易日 · 数据截至 %s", sum.Date, sum.DataDate), sections...)
}

// ==================== 日报 ====================

// buildDailyMailHTML 18:00 当天总结日报 (含当日告警汇总)
func buildDailyMailHTML(daily *api.DailyReportJSON, alerts []store.AgentAlert) string {
	var sections []string
	sections = append(sections, marketSection(daily.MarketSnapshot, "市场概况"))
	sections = append(sections, portfolioSection(daily.Portfolio, "持仓与资产"))
	sections = append(sections, rebalanceSection(daily))
	if daily.StrategyAdvice != nil {
		sa := daily.StrategyAdvice
		conf := int(sa.Confidence * 100)
		content := fmt.Sprintf(`<div style="font-size:13px;margin:2px 0;"><b>%s</b> <span style="color:#98a2b3;">(置信度 %d%%)</span></div><div style="font-size:13px;color:#555;margin:2px 0;">环境: %s</div><div style="font-size:13px;color:#555;margin:2px 0;">%s</div>`,
			esc(sa.Recommended), conf, esc(sa.Condition), esc(sa.Reason))
		sections = append(sections, mailSection("策略建议", content))
	}
	sections = append(sections, actionSection(daily.ActionItems))
	sections = append(sections, alertsSection(alerts))
	return mailLayout("惊蛰日报", daily.Date, sections...)
}

// ==================== 区块构建 ====================

// marketSection 市场概况
func marketSection(ms *api.MarketSnapshotJSON, title string) string {
	if ms == nil {
		return ""
	}
	kv := [][2]string{
		{"上涨", fmt.Sprintf(`<span style="color:#d63a3a;">%d</span>`, ms.UpCount)},
		{"下跌", fmt.Sprintf(`<span style="color:#1a8f4c;">%d</span>`, ms.DownCount)},
		{"涨停", fmt.Sprintf(`<span style="color:#d63a3a;">%d</span>`, ms.LimitUpCount)},
		{"跌停", fmt.Sprintf(`<span style="color:#1a8f4c;">%d</span>`, ms.LimitDownCount)},
		{"量比", fmt.Sprintf("%.2f", ms.VolumeRatio)},
	}
	return mailSection(title, mailKV(kv...))
}

// portfolioSection 持仓与资产
func portfolioSection(p *api.PortfolioJSON, title string) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	kv := [][2]string{
		{"总资产", fmt.Sprintf("¥%.2f", p.TotalAsset)},
		{"现金", fmt.Sprintf("¥%.2f", p.Cash)},
		{"当日盈亏", pnlSpan(p.DailyPnLPct)},
		{"健康度", fmt.Sprintf("%.0f/100", p.HealthScore)},
	}
	b.WriteString(mailKV(kv...))

	var rows [][]string
	for _, h := range p.Holdings {
		name, _ := h["name"].(string)
		qty, _ := h["total_qty"].(float64)
		price, _ := h["market_price"].(float64)
		mv, _ := h["market_value"].(float64)
		pnlPct, _ := h["pnl_pct"].(float64)
		weight, _ := h["weight_pct"].(float64)
		rows = append(rows, []string{
			esc(name), fmt.Sprintf("%d", int(qty)), fmt.Sprintf("%.2f", price),
			fmt.Sprintf("¥%.0f", mv), pnlSpan(pnlPct), fmt.Sprintf("%.1f%%", weight),
		})
	}
	if len(rows) == 0 {
		b.WriteString(`<div style="font-size:13px;color:#98a2b3;margin-top:6px;">当前无持仓</div>`)
		return mailSection(title, b.String())
	}
	b.WriteString(mailTable([]string{"名称", "数量", "现价", "市值", "盈亏", "占比"}, rows))
	return mailSection(title, b.String())
}

// planSection 交易计划 (盘前/日报共用)
func planSection(plans []store.TradePlan, title string) string {
	var rows [][]string
	for _, p := range plans {
		if p.Status == store.PlanStatusExpired {
			continue
		}
		rows = append(rows, []string{
			esc(p.Name), dirLabel(p.Direction), fmt.Sprintf("%d", p.Qty),
			fmt.Sprintf("%.2f", p.RefPrice), esc(p.Reason),
		})
	}
	if len(rows) == 0 {
		return mailSection(title, `<div style="font-size:13px;color:#98a2b3;">今日无交易计划</div>`)
	}
	return mailSection(title, mailTable([]string{"名称", "方向", "数量", "参考价", "原因"}, rows))
}

// goalSection 季度目标状态
func goalSection(g *goal.Status) string {
	ret := g.ReturnPct * 100
	kv := [][2]string{
		{"季度", fmt.Sprintf("%s (目标 %.1f%%)", esc(g.Quarter), g.TargetPct*100)},
		{"收益", fmt.Sprintf(`<span style="color:%s;font-weight:600;">%+.2f%%</span>`, pnlColor(ret), ret)},
		{"进度", fmt.Sprintf("%.0f%%", g.Progress*100)},
		{"回撤", fmt.Sprintf("%.2f%% / 预算 %.1f%% (消耗 %.0f%%)", g.DrawdownPct*100, g.BudgetPct*100, g.BudgetConsumed*100)},
		{"模式", esc(g.ModeLabel)},
	}
	return mailSection("季度目标", mailKV(kv...))
}

// rebalanceSection 调仓清单
func rebalanceSection(daily *api.DailyReportJSON) string {
	if daily.Rebalance == nil {
		return ""
	}
	rb := daily.Rebalance
	var rows [][]string
	for _, s := range rb.SellList {
		rows = append(rows, []string{esc(s.Name), `<span style="color:#1a8f4c;font-weight:600;">必卖</span>`, fmt.Sprintf("%d", s.DeltaQty), esc(s.Reason)})
	}
	for _, b := range rb.BuyList {
		rows = append(rows, []string{esc(b.Name), `<span style="color:#d63a3a;font-weight:600;">必买</span>`, fmt.Sprintf("%d @ %.2f", b.DeltaQty, b.Price), esc(b.Reason)})
	}
	for _, h := range rb.HoldList {
		if h.Suggestion != "继续持有" {
			rows = append(rows, []string{esc(h.Name), "持有提醒", "-", esc(h.Suggestion)})
		}
	}
	if len(rows) == 0 {
		return mailSection("调仓计划", `<div style="font-size:13px;color:#98a2b3;">无需调仓, 继续持有</div>`)
	}
	return mailSection("调仓计划", mailTable([]string{"名称", "操作", "数量", "原因"}, rows))
}

// actionSection 操作清单
func actionSection(items []api.ActionItemJSON) string {
	if len(items) == 0 {
		return ""
	}
	var rows [][]string
	for _, it := range items {
		name := it.Name
		if name == "" {
			name = it.Action
		}
		rows = append(rows, []string{esc(it.Time), esc(it.Action), esc(name), esc(it.Detail)})
	}
	return mailSection("操作清单", mailTable([]string{"时间", "动作", "标的", "说明"}, rows))
}

// alertsSection 当日告警汇总 (任务失败/数据更新/对账等过程通知)
func alertsSection(alerts []store.AgentAlert) string {
	if len(alerts) == 0 {
		return ""
	}
	levelColor := map[string]string{
		store.AlertLevelUrgent:  "#d63a3a",
		store.AlertLevelWarning: "#e08a00",
		store.AlertLevelSuccess: "#1a8f4c",
		store.AlertLevelInfo:    "#555555",
	}
	var rows [][]string
	for _, a := range alerts {
		color, ok := levelColor[a.Level]
		if !ok {
			color = "#555555"
		}
		content := a.Content
		if r := []rune(content); len(r) > 60 {
			content = string(r[:60]) + "..." // 按字符截断, 避免切断中文产生乱码
		}
		rows = append(rows, []string{
			fmt.Sprintf(`<span style="color:%s;font-weight:600;">%s</span>`, color, esc(a.Title)),
			esc(content),
		})
	}
	return mailSection("当日告警", mailTable([]string{"级别", "内容"}, rows))
}
