package mcp

import (
	"context"
	"testing"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// TestReportFillWiringUsesTodayForOpenDate 回归 2026-10-23 601328 现场：
// 指令单 10-22 生成、10-23 成交，MCP report_fill 的 date 参数仍可能指向前一天
// （用于按日匹配单据）。接线必须把"实际今天"作为 FillDate，否则持仓建仓日
// 少算一天，最小持有期保护跟着少算一天。
func TestReportFillWiringUsesTodayForOpenDate(t *testing.T) {
	ts, st, cleanup := newTestServerWithStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().In(market.Loc)
	yesterday := now.AddDate(0, 0, -1).Format("20060102")
	wantDate := today()
	if now.Format("20060102") != wantDate {
		t.Fatalf("测试与环境日期不一致：now=%s today()=%s", now.Format("20060102"), wantDate)
	}
	validUntil := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 0, 0, market.Loc).Format(time.RFC3339)

	id, err := st.TradeRepo().InsertTicket(ctx, model.OrderTicket{
		TradeDate: yesterday, TsCode: "601328.SH", Name: "交通银行", Direction: model.DirBuy,
		Qty: 100, RefPrice: model.FromFloat(8.26), Reason: "跨日单",
		Status: model.TicketIssued, ValidUntil: validUntil,
	})
	if err != nil {
		t.Fatalf("插入跨日指令单失败: %v", err)
	}

	// date 传前一天（匹配单据用的日期），成交日必须按实际今天落
	payload := mcpCallOK(t, ts, "report_fill", map[string]interface{}{
		"ticket_id": id, "date": yesterday, "qty": 100, "price": 8.26,
	})
	if payload["duplicate"] == true {
		t.Fatalf("首次回执不应幂等命中: %v", payload)
	}

	pos, err := st.TradeRepo().GetPosition(ctx, "601328.SH")
	if err != nil {
		t.Fatalf("读持仓失败: %v", err)
	}
	if pos.FirstOpenDate != wantDate {
		t.Errorf("建仓日=%q, 期望 %q（实际成交日，不是 date 参数 %q）", pos.FirstOpenDate, wantDate, yesterday)
	}
	if pos.TotalQty != 100 {
		t.Errorf("持仓=%d, 期望 100", int64(pos.TotalQty))
	}
}
