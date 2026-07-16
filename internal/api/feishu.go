package api

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ==================== 飞书卡片结构定义 ====================

// FeishuCard 飞书消息卡片
type FeishuCard struct {
	Config   FeishuCardConfig    `json:"config"`
	Header   *FeishuCardHeader   `json:"header,omitempty"`
	Elements []FeishuCardElement `json:"elements"`
}

// FeishuCardConfig 卡片配置
type FeishuCardConfig struct {
	WideScreenMode bool `json:"wide_screen_mode"`
	EnableForward  bool `json:"enable_forward"`
}

// FeishuCardHeader 卡片头部
type FeishuCardHeader struct {
	Title    *FeishuCardTitle `json:"title"`
	Template string           `json:"template"` // red/orange/green/blue/grey
}

// FeishuCardTitle 卡片标题
type FeishuCardTitle struct {
	Tag     string `json:"tag"`
	Content string `json:"content"`
}

// FeishuCardElement 卡片元素 (支持多种类型)
type FeishuCardElement struct {
	Tag    string          `json:"tag"`               // "div" / "field" / "note" / "action"
	Text   *FeishuText     `json:"text,omitempty"`
	Fields []FeishuField   `json:"fields,omitempty"`
	Action *FeishuAction   `json:"action,omitempty"`
}

// FeishuAction 卡片动作按钮
type FeishuAction struct {
	Tag  string       `json:"tag"` // "button"
	Text *FeishuText  `json:"text"`
	URL  string       `json:"url,omitempty"`
	Type string       `json:"type"` // "primary" / "default" / "danger"
}

// FeishuText 富文本
type FeishuText struct {
	Tag     string `json:"tag"`     // "lark_md"
	Content string `json:"content"`
}

// FeishuField 字段组
type FeishuField struct {
	IsShort bool        `json:"is_short"`
	Text    *FeishuText `json:"text"`
}

// ToJSON 序列化为飞书 API 需要的 JSON
func (c *FeishuCard) ToJSON() []byte {
	body, _ := json.MarshalIndent(c, "", "  ")
	return body
}

// ==================== 卡片构建方法 ====================

// BuildFeishuDailyCard 从 DailyReportJSON 构建飞书消息卡片 (完整版)
func BuildFeishuDailyCard(report *DailyReportJSON) *FeishuCard {
	card := &FeishuCard{
		Config: FeishuCardConfig{
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

	card.Header = &FeishuCardHeader{
		Title:    &FeishuCardTitle{Tag: "plain_text", Content: headerTitle},
		Template: headerTemplate,
	}

	// 2. 市场概况
	if report.MarketSnapshot != nil {
		ms := report.MarketSnapshot
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag: "div",
			Fields: []FeishuField{
				{
					IsShort: true,
					Text:    mdText(fmt.Sprintf("**上涨** %d 家", ms.UpCount)),
				},
				{
					IsShort: true,
					Text:    mdText(fmt.Sprintf("**下跌** %d 家", ms.DownCount)),
				},
				{
					IsShort: true,
					Text:    mdText(fmt.Sprintf("**涨停** %d", ms.LimitUpCount)),
				},
				{
					IsShort: true,
					Text:    mdText(fmt.Sprintf("**跌停** %d", ms.LimitDownCount)),
				},
				{
					IsShort: true,
					Text:    mdText(fmt.Sprintf("**量比** %.2f", ms.VolumeRatio)),
				},
			},
		})

		// 热点板块
		if len(ms.HotSectors) > 0 {
			var sectorLines []string
			for _, hs := range ms.HotSectors {
				sector := hs["sector"].(string)
				avgChange := hs["avg_change"].(float64)
				leader := hs["leader_stock"].(string)
				leaderChange := hs["leader_change"].(float64)
				sectorLines = append(sectorLines, fmt.Sprintf("- %s 均涨幅%+.2f%% 领涨:%s(%+.2f%%)",
					sector, avgChange, leader, leaderChange))
			}
			if len(sectorLines) > 3 {
				sectorLines = sectorLines[:3]
			}
			card.Elements = append(card.Elements, FeishuCardElement{
				Tag:  "div",
				Text: mdText("**热点板块**\n" + strings.Join(sectorLines, "\n")),
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
			card.Elements = append(card.Elements, FeishuCardElement{
				Tag:  "div",
				Text: mdText("**市场告警**\n" + strings.Join(alarmLines, "\n")),
			})
		}
	}

	// 3. 持仓健康度
	if report.Portfolio != nil {
		p := report.Portfolio
		healthColor := "🟢"
		if p.HealthScore < 60 {
			healthColor = "🔴"
		} else if p.HealthScore < 80 {
			healthColor = "🟡"
		}
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag: "div",
			Fields: []FeishuField{
				{
					IsShort: true,
					Text:    mdText(fmt.Sprintf("总资产 ¥%.2f", p.TotalAsset)),
				},
				{
					IsShort: true,
					Text:    mdText(fmt.Sprintf("健康度 %s **%.0f/100**", healthColor, p.HealthScore)),
				},
			},
		})
	}

	// 4. 策略建议
	if report.StrategyAdvice != nil {
		sa := report.StrategyAdvice
		confidencePct := int(sa.Confidence * 100)
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "div",
			Text: mdText(fmt.Sprintf("**策略建议**: %s (置信度 %d%%)\n环境: %s\n%s",
				sa.Recommended, confidencePct, sa.Condition, sa.Reason)),
		})
	}

	// 5. 必卖清单
	if report.Rebalance != nil && len(report.Rebalance.SellList) > 0 {
		var lines []string
		for _, sell := range report.Rebalance.SellList {
			lines = append(lines, fmt.Sprintf("- <font color='red'>%s</font> %d股 %s",
				sell.Name, sell.DeltaQty, sell.Reason))
		}
		if len(lines) > 5 {
			lines = lines[:5]
			lines = append(lines, "...")
		}
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "div",
			Text: mdText("**必卖清单**\n" + strings.Join(lines, "\n")),
		})
	}

	// 6. 必买清单
	if report.Rebalance != nil && len(report.Rebalance.BuyList) > 0 {
		var lines []string
		for _, buy := range report.Rebalance.BuyList {
			lines = append(lines, fmt.Sprintf("- <font color='green'>%s</font> %d股 @%.2f %s",
				buy.Name, buy.DeltaQty, buy.Price, buy.Reason))
		}
		if len(lines) > 5 {
			lines = lines[:5]
			lines = append(lines, "...")
		}
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "div",
			Text: mdText("**必买清单**\n" + strings.Join(lines, "\n")),
		})
	}

	// 7. 持有提醒
	if report.Rebalance != nil && len(report.Rebalance.HoldList) > 0 {
		var lines []string
		for _, hold := range report.Rebalance.HoldList {
			if hold.Suggestion != "继续持有" {
				icon := "👀"
				if strings.Contains(hold.Suggestion, "止损") {
					icon = "⚠️"
				} else if strings.Contains(hold.Suggestion, "止盈") {
					icon = "🎯"
				}
				lines = append(lines, fmt.Sprintf("%s %s - %s", icon, hold.Name, hold.Suggestion))
			}
		}
		if len(lines) > 0 {
			if len(lines) > 5 {
				lines = lines[:5]
			}
			card.Elements = append(card.Elements, FeishuCardElement{
				Tag:  "div",
				Text: mdText("**持有提醒**\n" + strings.Join(lines, "\n")),
			})
		}
	}

	// 8. 明日预案 (尾部)
	var tomorrowParts []string
	if report.StrategyAdvice != nil {
		tomorrowParts = append(tomorrowParts, report.StrategyAdvice.Reason)
	}
	if report.Rebalance != nil && report.Rebalance.Reason != "" {
		tomorrowParts = append(tomorrowParts, "调仓: "+report.Rebalance.Reason)
	}
	if len(tomorrowParts) > 0 {
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "note",
			Text: mdText("明日预案: " + strings.Join(tomorrowParts, "; ")),
		})
	}

	return card
}

// BuildFeishuTradeCard 构建买卖清单卡片 (简洁版, 适合盘中推送)
func BuildFeishuTradeCard(report *DailyReportJSON) *FeishuCard {
	card := &FeishuCard{
		Config: FeishuCardConfig{
			WideScreenMode: true,
			EnableForward:  true,
		},
		Header: &FeishuCardHeader{
			Title:    &FeishuCardTitle{Tag: "plain_text", Content: fmt.Sprintf("交易提醒 %s", formatDate(report.Date))},
			Template: "blue",
		},
	}

	if report.Rebalance == nil {
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "div",
			Text: mdText("当前无调仓信号, 建议持仓不动"),
		})
		return card
	}

	// 卖出清单
	if len(report.Rebalance.SellList) > 0 {
		var lines []string
		for _, sell := range report.Rebalance.SellList {
			lines = append(lines, fmt.Sprintf("- **%s** %d股 %.2f [%s]",
				sell.Name, sell.DeltaQty, sell.Price, sell.Urgency))
		}
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "div",
			Text: mdText("<font color='red'>**卖出**</font>\n" + strings.Join(lines, "\n")),
		})
	}

	// 买入清单
	if len(report.Rebalance.BuyList) > 0 {
		var lines []string
		for _, buy := range report.Rebalance.BuyList {
			lines = append(lines, fmt.Sprintf("- **%s** %d股 %.2f [%s]",
				buy.Name, buy.DeltaQty, buy.Price, buy.Urgency))
		}
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "div",
			Text: mdText("<font color='green'>**买入**</font>\n" + strings.Join(lines, "\n")),
		})
	}

	if len(report.Rebalance.SellList) == 0 && len(report.Rebalance.BuyList) == 0 {
		card.Elements = append(card.Elements, FeishuCardElement{
			Tag:  "div",
			Text: mdText("当前无调仓信号, 建议持仓不动"),
		})
	}

	return card
}

// mdText 创建 lark_md 富文本
func mdText(content string) *FeishuText {
	return &FeishuText{
		Tag:     "lark_md",
		Content: content,
	}
}

// plainText 创建纯文本
func plainText(content string) *FeishuText {
	return &FeishuText{
		Tag:     "plain_text",
		Content: content,
	}
}