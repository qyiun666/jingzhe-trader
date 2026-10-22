package mcp

import (
	"context"
	"testing"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// TestBriefTicketsPendingIncludesUnexpiredFromPreviousDay 回归：前一晚签发、有效期覆盖今日的
// 指令单必须计入 tickets_pending。按当日 trade_date 数会把昨晚的单显示成 0（2026-10-23 组长指出）。
func TestBriefTicketsPendingIncludesUnexpiredFromPreviousDay(t *testing.T) {
	ts, st, cleanup := newTestServerWithStore(t)
	defer cleanup()
	ctx := context.Background()

	now := time.Now().In(market.Loc)
	yesterday := now.AddDate(0, 0, -1).Format("20060102")
	laterToday := time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 0, 0, market.Loc).Format(time.RFC3339)
	expired := now.AddDate(0, 0, -1).Format(time.RFC3339)

	mustInsert := func(code, validUntil string, status model.TicketStatus) {
		t.Helper()
		if _, err := st.TradeRepo().InsertTicket(ctx, model.OrderTicket{
			TradeDate: yesterday, TsCode: code, Name: "测试股", Direction: model.DirBuy,
			Qty: 100, RefPrice: model.FromFloat(10), Reason: "跨日单",
			Status: status, ValidUntil: validUntil,
		}); err != nil {
			t.Fatalf("插入指令单失败: %v", err)
		}
	}
	mustInsert("600519.SH", laterToday, model.TicketIssued) // 昨晚签发、今日仍有效
	mustInsert("600520.SH", expired, model.TicketIssued)    // 已过期，不应计入

	payload := mcpCallOK(t, ts, "get_brief", map[string]interface{}{})
	if got := payload["tickets_pending"]; got != float64(1) {
		t.Errorf("tickets_pending=%v，期望 1（只数活跃且未过期，过期单排除）", got)
	}
}
