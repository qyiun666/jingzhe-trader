package agent

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// formatMoneyFlows 近 n 个交易日资金流文本 (最新在前); 空数据返回占位文案
func formatMoneyFlows(flows []model.MoneyFlow, n int) string {
	if len(flows) == 0 {
		return "无数据"
	}
	var sb strings.Builder
	start := 0
	if len(flows) > n {
		start = len(flows) - n
	}
	for i := len(flows) - 1; i >= start; i-- {
		f := flows[i]
		sb.WriteString(fmt.Sprintf("  %s 净流入%.0f万 (超大单买%.0f万/卖%.0f万)\n",
			f.TradeDate, f.NetMFAmount, f.BuyElgAmount, f.SellElgAmount))
	}
	return sb.String()
}

// formatTopLists 龙虎榜文本 (最新在前); 空数据返回占位文案
func formatTopLists(lists []model.TopList, n int) string {
	if len(lists) == 0 {
		return "无"
	}
	var sb strings.Builder
	start := 0
	if len(lists) > n {
		start = len(lists) - n
	}
	for i := len(lists) - 1; i >= start; i-- {
		t := lists[i]
		sb.WriteString(fmt.Sprintf("  %s 上榜 涨跌%.2f%% 净买入%.0f万\n",
			t.TradeDate, t.PctChange, t.NetAmount))
	}
	return sb.String()
}

// formatReviews 历史辩论复盘文本 (反思注入); 空数据返回空串 (调用方据此省略整个小节)
func formatReviews(reviews []store.DebateReview, n int) string {
	if len(reviews) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range reviews {
		if i >= n {
			break
		}
		verdict := "✗"
		if r.Correct == 1 {
			verdict = "✓"
		}
		sb.WriteString(fmt.Sprintf("  %s 决策=%s → 后续%.2f%% %s\n",
			r.TradeDate, r.Decision, r.RetPct, verdict))
	}
	return sb.String()
}
