package api

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/notify"
)

// 卡片结构与发送器统一定义在 notify 包, 此处仅保留业务卡片构建器

// BuildFeishuDailyCard 从 DailyReportJSON 构建飞书消息卡片 (完整版)
func BuildFeishuDailyCard(report *DailyReportJSON) *notify.FeishuCard {
	card := &notify.FeishuCard{
		Config: notify.FeishuCardConfig{
			WideScreenMode: true,
			EnableForward:  true,
		},
	}

	// 1. 头部: 根据盈亏用不同颜色
	displayDate := formatDate(report.Date)
	totalPnL := 0.0
	if report.Portfolio != nil {
		if pnl, ok := report.Portfolio.PnLSummary["total_pnl"]; ok {
			totalPnL, _ = pnl.(float64)
		}
	}

	headerTemplate := "blue"
	headerTitle := fmt.Sprintf("操盘报告 %s", displayDate)
	if totalPnL > 0 {
		headerTemplate = "green"
		headerTitle = fmt.Sprintf("操盘报告 %s (浮盈)", displayDate)
	} else if totalPnL < 0 {
		headerTemplate = "red"
		headerTitle = fmt.Sprintf("操盘报告 %s (浮亏)", displayDate)
	}

	card.Header = &notify.FeishuCardHeader{
		Title:    &notify.FeishuCardTitle{Tag: "plain_text", Content: headerTitle},
		Template: headerTemplate,
	}

	appendMarketElements(card, report)
	appendPortfolioElements(card, report)
	appendRebalanceElements(card, report)
	appendTomorrowNote(card, report)
	return card
}

// appendMarketElements 追加市场概况/热点/告警元素
func appendMarketElements(card *notify.FeishuCard, report *DailyReportJSON) {
	if report.MarketSnapshot == nil {
		return
	}
	ms := report.MarketSnapshot
	card.Elements = append(card.Elements, notify.FeishuCardElement{
		Tag: "div",
		Fields: []notify.FeishuField{
			{IsShort: true, Text: notify.MdText(fmt.Sprintf("**上涨** %d 家", ms.UpCount))},
			{IsShort: true, Text: notify.MdText(fmt.Sprintf("**下跌** %d 家", ms.DownCount))},
			{IsShort: true, Text: notify.MdText(fmt.Sprintf("**涨停** %d", ms.LimitUpCount))},
			{IsShort: true, Text: notify.MdText(fmt.Sprintf("**跌停** %d", ms.LimitDownCount))},
			{IsShort: true, Text: notify.MdText(fmt.Sprintf("**量比** %.2f", ms.VolumeRatio))},
		},
	})

	// 热点板块
	if len(ms.HotSectors) > 0 {
		var sectorLines []string
		for _, hs := range ms.HotSectors {
			sector, _ := hs["sector"].(string)
			avgChange, _ := hs["avg_change"].(float64)
			leader, _ := hs["leader_stock"].(string)
			leaderChange, _ := hs["leader_change"].(float64)
			sectorLines = append(sectorLines, fmt.Sprintf("- %s 均涨幅%+.2f%% 领涨:%s(%+.2f%%)",
				sector, avgChange, leader, leaderChange))
		}
		if len(sectorLines) > 3 {
			sectorLines = sectorLines[:3]
		}
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag:  "div",
			Text: notify.MdText("**热点板块**\n" + strings.Join(sectorLines, "\n")),
		})
	}

	// 告警
	if len(ms.Alarms) > 0 {
		var alarmLines []string
		for _, alarm := range ms.Alarms {
			icon := "🔔"
			if alarm["level"] == "danger" {
				icon = "🚨"
			} else if alarm["level"] == "warning" {
				icon = "⚠️"
			}
			alarmLines = append(alarmLines, fmt.Sprintf("%s %s", icon, alarm["message"]))
		}
		if len(alarmLines) > 5 {
			alarmLines = alarmLines[:5]
			alarmLines = append(alarmLines, "...")
		}
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag:  "div",
			Text: notify.MdText("**市场告警**\n" + strings.Join(alarmLines, "\n")),
		})
	}
}

// appendPortfolioElements 追加持仓健康度与策略建议元素
func appendPortfolioElements(card *notify.FeishuCard, report *DailyReportJSON) {
	if report.Portfolio != nil {
		p := report.Portfolio
		healthColor := "🟢"
		if p.HealthScore < 60 {
			healthColor = "🔴"
		} else if p.HealthScore < 80 {
			healthColor = "🟡"
		}
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag: "div",
			Fields: []notify.FeishuField{
				{IsShort: true, Text: notify.MdText(fmt.Sprintf("总资产 ¥%.2f", p.TotalAsset))},
				{IsShort: true, Text: notify.MdText(fmt.Sprintf("健康度 %s **%.0f/100**", healthColor, p.HealthScore))},
			},
		})
	}

	if report.StrategyAdvice != nil {
		sa := report.StrategyAdvice
		confidencePct := int(sa.Confidence * 100)
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag: "div",
			Text: notify.MdText(fmt.Sprintf("**策略建议**: %s (置信度 %d%%)\n环境: %s\n%s",
				sa.Recommended, confidencePct, sa.Condition, sa.Reason)),
		})
	}
}

// appendRebalanceElements 追加必卖/必买/持有提醒元素
func appendRebalanceElements(card *notify.FeishuCard, report *DailyReportJSON) {
	if report.Rebalance == nil {
		return
	}
	if len(report.Rebalance.SellList) > 0 {
		var lines []string
		for _, sell := range report.Rebalance.SellList {
			lines = append(lines, fmt.Sprintf("- <font color='red'>%s</font> %d股 %s",
				sell.Name, sell.DeltaQty, sell.Reason))
		}
		if len(lines) > 5 {
			lines = append(lines[:5], "...")
		}
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag:  "div",
			Text: notify.MdText("**必卖清单**\n" + strings.Join(lines, "\n")),
		})
	}

	if len(report.Rebalance.BuyList) > 0 {
		var lines []string
		for _, buy := range report.Rebalance.BuyList {
			lines = append(lines, fmt.Sprintf("- <font color='green'>%s</font> %d股 @%.2f %s",
				buy.Name, buy.DeltaQty, buy.Price, buy.Reason))
		}
		if len(lines) > 5 {
			lines = append(lines[:5], "...")
		}
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag:  "div",
			Text: notify.MdText("**必买清单**\n" + strings.Join(lines, "\n")),
		})
	}

	var holdLines []string
	for _, hold := range report.Rebalance.HoldList {
		if hold.Suggestion != "继续持有" {
			icon := "👀"
			if strings.Contains(hold.Suggestion, "止损") {
				icon = "⚠️"
			} else if strings.Contains(hold.Suggestion, "止盈") {
				icon = "🎯"
			}
			holdLines = append(holdLines, fmt.Sprintf("%s %s - %s", icon, hold.Name, hold.Suggestion))
		}
	}
	if len(holdLines) > 0 {
		if len(holdLines) > 5 {
			holdLines = holdLines[:5]
		}
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag:  "div",
			Text: notify.MdText("**持有提醒**\n" + strings.Join(holdLines, "\n")),
		})
	}
}

// appendTomorrowNote 追加明日预案备注
func appendTomorrowNote(card *notify.FeishuCard, report *DailyReportJSON) {
	var tomorrowParts []string
	if report.StrategyAdvice != nil {
		tomorrowParts = append(tomorrowParts, report.StrategyAdvice.Reason)
	}
	if report.Rebalance != nil && report.Rebalance.Reason != "" {
		tomorrowParts = append(tomorrowParts, "调仓: "+report.Rebalance.Reason)
	}
	if len(tomorrowParts) > 0 {
		card.Elements = append(card.Elements, notify.FeishuCardElement{
			Tag:  "note",
			Text: notify.MdText("明日预案: " + strings.Join(tomorrowParts, "; ")),
		})
	}
}
