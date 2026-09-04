package store

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
)

// seedTicketRow 落一张指令单（只填必填列 + 状态与有效期，别的列交给默认值）。
func seedTicketRow(t *testing.T, s *Store, code, date, status, validUntil string) int64 {
	t.Helper()
	id, err := s.TradeRepo().InsertTicket(context.Background(), model.OrderTicket{
		TradeDate: date, TsCode: code, Name: "测试" + code, Direction: model.DirBuy,
		Qty: 100, RefPrice: model.FromFloat(10), Reason: "单测种子",
		Status: model.TicketStatus(status), ValidUntil: validUntil, Gear: model.GearG1,
	})
	if err != nil {
		t.Fatalf("落种子指令单失败: %v", err)
	}
	return id
}

// TestExpireStale 过了有效期仍未执行的单必须收口成 expired；
// 未到期、已成交、已作废的都不许被动到。
func TestExpireStale(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	due := seedTicketRow(t, s, "600001.SH", "20260901", "issued", "2026-09-02T15:00:00+08:00")
	live := seedTicketRow(t, s, "600002.SH", "20260903", "drafted", "2026-09-04T15:00:00+08:00")
	filled := seedTicketRow(t, s, "600003.SH", "20260901", "filled", "2026-09-02T15:00:00+08:00")

	n, err := s.TradeRepo().ExpireStale(ctx, "2026-09-03T09:00:00+08:00")
	if err != nil {
		t.Fatalf("ExpireStale 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("只应收掉 1 张过期未执行单，实际 %d", n)
	}
	status := func(id int64) string {
		var st string
		if err := s.readDB.QueryRowContext(ctx, "SELECT status FROM order_ticket WHERE id=?", id).Scan(&st); err != nil {
			t.Fatalf("读 %d 状态失败: %v", id, err)
		}
		return st
	}
	if got := status(due); got != "expired" {
		t.Errorf("过期的单 status=%q，期望 expired", got)
	}
	if got := status(live); got != "drafted" {
		t.Errorf("未到期的单被改成了 %q", got)
	}
	if got := status(filled); got != "filled" {
		t.Errorf("已成交的单被改成了 %q", got)
	}
}

// TestExpireStaleNothingToDo 没有过期单时返回 0 且不写任何行。
func TestExpireStaleNothingToDo(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	seedTicketRow(t, s, "600001.SH", "20260903", "issued", "2026-09-04T15:00:00+08:00")
	n, err := s.TradeRepo().ExpireStale(context.Background(), "2026-09-03T09:00:00+08:00")
	if err != nil || n != 0 {
		t.Fatalf("未到期时不该收单：n=%d err=%v", n, err)
	}
}
