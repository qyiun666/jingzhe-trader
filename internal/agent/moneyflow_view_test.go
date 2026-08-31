package agent

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

func TestFormatMoneyFlows(t *testing.T) {
	if got := formatMoneyFlows(nil, 5); got != "无数据" {
		t.Fatalf("空数据应返回无数据, got %q", got)
	}
	flows := []model.MoneyFlow{
		{TradeDate: "20260825", BuyElgAmount: 900, SellElgAmount: 400, NetMFAmount: 500},
		{TradeDate: "20260826", BuyElgAmount: 800, SellElgAmount: 900, NetMFAmount: -100},
	}
	got := formatMoneyFlows(flows, 5)
	if !strings.Contains(got, "20260826") || strings.Count(got, "\n") != 2 {
		t.Fatalf("应含2条记录且最新在前: %q", got)
	}
	if strings.Contains(got, "20260825\n") {
		t.Fatalf("最新日期应在最前: %q", got)
	}
}

func TestFormatTopLists(t *testing.T) {
	if got := formatTopLists(nil, 3); got != "无" {
		t.Fatalf("空数据应返回无, got %q", got)
	}
	// 库内 net_amount 单位为元: 125186146 元应展示为 12519 万
	lists := []model.TopList{{TradeDate: "20260820", PctChange: 9.98, NetAmount: 125186146}}
	got := formatTopLists(lists, 3)
	if !strings.Contains(got, "上榜") || !strings.Contains(got, "净买入12519万") {
		t.Fatalf("金额应按元→万元换算: %q", got)
	}
}

func TestFormatReviews(t *testing.T) {
	if got := formatReviews(nil, 5); got != "" {
		t.Fatalf("空复盘应返回空串, got %q", got)
	}
	reviews := []store.DebateReview{
		{TradeDate: "20260818", Decision: "buy", RetPct: 3.2, Correct: 1},
		{TradeDate: "20260820", Decision: "buy", RetPct: -1.5, Correct: 0},
	}
	got := formatReviews(reviews, 5)
	if !strings.Contains(got, "✓") || !strings.Contains(got, "✗") {
		t.Fatalf("应含对错标记: %q", got)
	}
}
