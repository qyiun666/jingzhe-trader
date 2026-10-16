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

// TestUpsertPositionFirstOpenDateMonotonic first_open_date 只写不擦：空值表示"不知道建仓日"，
// 绝不允许把库里既有的日期清掉（agent_issues #12-B：组合同步整列覆盖 → 持有期算成未知 →
// 最小持有期保护失效 → 每天生成淘汰刚买入持仓的错误卖出单）。改日期必须显式给新值。
func TestUpsertPositionFirstOpenDateMonotonic(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	code := "600704.SH"

	write := func(total model.Qty, openDate string) {
		t.Helper()
		if err := s.TradeRepo().UpsertPosition(ctx, model.Position{
			TsCode: code, TotalQty: total, CostPrice: model.FromFloat(5),
			HighPrice: model.FromFloat(5), FirstOpenDate: openDate,
		}); err != nil {
			t.Fatalf("写持仓失败: %v", err)
		}
	}

	write(200, "20261012")
	if got := mustPosition(t, s, code).FirstOpenDate; got != "20261012" {
		t.Fatalf("首次写入建仓日=%q, 期望 20261012", got)
	}
	// 空建仓日的同步（券商口径）不得擦掉既有日期，其余列照常更新
	write(300, "")
	p := mustPosition(t, s, code)
	if p.FirstOpenDate != "20261012" {
		t.Errorf("空值覆盖后建仓日=%q, 期望仍为 20261012", p.FirstOpenDate)
	}
	if p.TotalQty != 300 {
		t.Errorf("其余列应照常更新: total=%d, 期望 300", int64(p.TotalQty))
	}
	// 显式给新值才改（清仓后重开、人工纠错走的都是这条路）
	write(100, "20261015")
	if got := mustPosition(t, s, code).FirstOpenDate; got != "20261015" {
		t.Errorf("显式改建仓日失败: %q, 期望 20261015", got)
	}
	// 库里是 NULL 的历史行：读出来是空串，写空值不报错也不改变
	if _, err := s.WriteDB().Exec(`UPDATE position SET first_open_date=NULL WHERE ts_code=?`, code); err != nil {
		t.Fatalf("构造 NULL 建仓日失败: %v", err)
	}
	write(100, "")
	if got := mustPosition(t, s, code).FirstOpenDate; got != "" {
		t.Errorf("NULL 行写空值后=%q, 期望空串", got)
	}
	write(100, "20261016")
	if got := mustPosition(t, s, code).FirstOpenDate; got != "20261016" {
		t.Errorf("NULL 行显式补日期失败: %q", got)
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
