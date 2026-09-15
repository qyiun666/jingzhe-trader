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
		Status: model.TicketStatus(status), ValidUntil: validUntil,
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

// TestRaiseHighPriceMonotonic 期间最高价只升不降：较小值不得覆盖较大值，
// 非正值直接忽略（没有可用价的持仓不该被写坏）。
func TestRaiseHighPriceMonotonic(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	code := "600000.SH"

	if err := s.TradeRepo().UpsertPosition(ctx, model.Position{
		TsCode: code, TotalQty: 100, CostPrice: model.FromFloat(10), HighPrice: model.FromFloat(10),
	}); err != nil {
		t.Fatalf("写持仓失败: %v", err)
	}

	// 抬到 12
	if err := s.TradeRepo().RaiseHighPrice(ctx, code, model.FromFloat(12)); err != nil {
		t.Fatalf("抬升高点失败: %v", err)
	}
	if got := mustPosition(t, s, code).HighPrice; got != model.FromFloat(12) {
		t.Errorf("高点=%s, 期望 12", got)
	}
	// 用更低的 11：不得覆盖
	if err := s.TradeRepo().RaiseHighPrice(ctx, code, model.FromFloat(11)); err != nil {
		t.Fatalf("抬升高点失败: %v", err)
	}
	if got := mustPosition(t, s, code).HighPrice; got != model.FromFloat(12) {
		t.Errorf("高点被降成 %s, 期望仍为 12（只升不降）", got)
	}
	// 非正值忽略
	if err := s.TradeRepo().RaiseHighPrice(ctx, code, 0); err != nil {
		t.Fatalf("非正价不该报错: %v", err)
	}
	if got := mustPosition(t, s, code).HighPrice; got != model.FromFloat(12) {
		t.Errorf("非正价改动了高点: %s", got)
	}
}

// mustPosition 读取持仓（测试辅助）。
func mustPosition(t *testing.T, s *Store, code string) model.Position {
	t.Helper()
	p, err := s.TradeRepo().GetPosition(context.Background(), code)
	if err != nil {
		t.Fatalf("读持仓 %s 失败: %v", code, err)
	}
	return p
}

// TestListActiveUnexpiredCrossDay 跨日取"活跃且未过期"的单：昨晚生成的单
//（trade_date=昨）在今天早上必须能被查到，而按当天 trade_date 过滤会漏掉它（历史缺陷）；
// 已过期 / 已成交的单都不许出现。
func TestListActiveUnexpiredCrossDay(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	// 昨晚 16:30 生成、次日 15:00 到期 → 今天早上应可见
	tonight := seedTicketRow(t, s, "600001.SH", "20260902", "issued", "2026-09-03T15:00:00+08:00")
	// 今天仍未到期
	today := seedTicketRow(t, s, "600002.SH", "20260903", "drafted", "2026-09-04T15:00:00+08:00")
	// 昨天就过期了
	stale := seedTicketRow(t, s, "600003.SH", "20260901", "issued", "2026-09-02T15:00:00+08:00")
	// 已成交
	seedTicketRow(t, s, "600004.SH", "20260902", "filled", "2026-09-03T15:00:00+08:00")

	// 当日早上 09:00（在今晚那张 15:00 到期之前）
	got, err := s.TradeRepo().ListActiveUnexpired(ctx, "2026-09-03T09:00:00+08:00")
	if err != nil {
		t.Fatalf("ListActiveUnexpired 失败: %v", err)
	}
	ids := map[int64]bool{}
	for _, t2 := range got {
		ids[t2.ID] = true
	}
	if !ids[tonight] {
		t.Errorf("昨晚生成的单（跨日未过期）应可见，实际未返回")
	}
	if !ids[today] {
		t.Errorf("今日未到期单应可见")
	}
	if ids[stale] {
		t.Errorf("已过期的单不应出现")
	}
	if len(got) != 2 {
		t.Errorf("应恰好 2 张活跃未过期单，实际 %d", len(got))
	}
}
