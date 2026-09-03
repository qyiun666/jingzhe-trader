package dataloader

import (
	"context"
	"fmt"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// 测试中使用的低阈值，便于以少量数据触发/通过行数检查。
const testMinBarRows = 3

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func findItem(rep *FreshnessReport, name string) (CheckItem, bool) {
	for _, c := range rep.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return CheckItem{}, false
}

// 每个测试仅断言"被设计触发失败的那一项"的失败码；其余项状态不影响断言。
// 仅正向用例断言 Fresh。

func mkBar(tsCode, date string) model.Bar {
	return model.Bar{
		TsCode: tsCode, TradeDate: date,
		Open: model.FromFloat(100), High: model.FromFloat(101), Low: model.FromFloat(99),
		Close: model.FromFloat(100), PreClose: model.FromFloat(100), RawClose: model.FromFloat(100), AdjFactor: 1,
	}
}

func TestFreshness_CalMissing(t *testing.T) {
	st := openTestStore(t)
	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), "20260901")
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, ok := findItem(rep, "CalendarOK")
	if !ok {
		t.Fatal("CalendarOK 检查缺失")
	}
	if c.OK || c.Code != CodeCalMissing {
		t.Fatalf("期望 CalendarOK=%s，实际 OK=%v Code=%q", CodeCalMissing, c.OK, c.Code)
	}
}

func TestFreshness_NotTradeDay(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	// 目标日在日历中但非开市
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: "20260901", IsOpen: false, Exchange: "SSE"})

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), "20260901")
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "IsTradeDay")
	if c.OK || c.Code != CodeNotTradeDay {
		t.Fatalf("期望 IsTradeDay=%s，实际 OK=%v Code=%q", CodeNotTradeDay, c.OK, c.Code)
	}
	if !rep.Skipped {
		t.Fatal("期望非交易日 Skipped=true")
	}
}

func TestFreshness_BarStale(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "BarDate")
	if c.OK || c.Code != CodeBarStale {
		t.Fatalf("期望 BarDate=%s，实际 OK=%v Code=%q", CodeBarStale, c.OK, c.Code)
	}
}

func TestFreshness_BarRowsLow(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})
	// 仅 1 行（< 阈值 3）
	_ = rc.UpsertBar(context.Background(), mkBar("600519.SH", tradeDate))

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "BarRows")
	if c.OK || c.Code != CodeBarRowsLow {
		t.Fatalf("期望 BarRows=%s，实际 OK=%v Code=%q", CodeBarRowsLow, c.OK, c.Code)
	}
}

func TestFreshness_BasicRowsLow(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})
	_ = rc.UpsertBar(context.Background(), mkBar("600519.SH", tradeDate))
	_ = rc.UpsertBar(context.Background(), mkBar("600520.SH", tradeDate))
	_ = rc.UpsertBar(context.Background(), mkBar("600521.SH", tradeDate))
	// 每日指标仅 1 行（< 阈值 3）
	_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: "600519.SH", TradeDate: tradeDate, Close: model.FromFloat(100)})

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "BasicRows")
	if c.OK || c.Code != CodeBasicRowsLow {
		t.Fatalf("期望 BasicRows=%s，实际 OK=%v Code=%q", CodeBasicRowsLow, c.OK, c.Code)
	}
}

func TestFreshness_LimitRowsLow(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})
	for i := 0; i < testMinBarRows; i++ {
		code := mkCode(i)
		_ = rc.UpsertBar(context.Background(), mkBar(code, tradeDate))
		_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: code, TradeDate: tradeDate, Close: model.FromFloat(100)})
	}
	// 涨跌停仅 1 行（< 阈值 3）
	_ = rc.UpsertLimit(context.Background(), model.PriceLimit{TsCode: "600519.SH", TradeDate: tradeDate, UpLimit: model.FromFloat(110), DownLimit: model.FromFloat(90)})

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "LimitRows")
	if c.OK || c.Code != CodeLimitRowsLow {
		t.Fatalf("期望 LimitRows=%s，实际 OK=%v Code=%q", CodeLimitRowsLow, c.OK, c.Code)
	}
}

func TestFreshness_CoverageGap(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})
	// 候选池 3 只（list_status='L'），但仅 2 只有日线 → 覆盖缺口
	cands := []string{"600519.SH", "600520.SH", "600521.SH"}
	for _, c := range cands {
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: c, ListStatus: "L", UpdatedAt: "20260901"})
	}
	_ = rc.UpsertBar(context.Background(), mkBar("600519.SH", tradeDate))
	_ = rc.UpsertBar(context.Background(), mkBar("600520.SH", tradeDate))
	_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: "600519.SH", TradeDate: tradeDate, Close: model.FromFloat(100)})
	_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: "600520.SH", TradeDate: tradeDate, Close: model.FromFloat(100)})
	_ = rc.UpsertLimit(context.Background(), model.PriceLimit{TsCode: "600519.SH", TradeDate: tradeDate, UpLimit: model.FromFloat(110), DownLimit: model.FromFloat(90)})
	_ = rc.UpsertLimit(context.Background(), model.PriceLimit{TsCode: "600520.SH", TradeDate: tradeDate, UpLimit: model.FromFloat(110), DownLimit: model.FromFloat(90)})

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "MissingCodes")
	if c.OK || c.Code != CodeCoverageGap {
		t.Fatalf("期望 MissingCodes=%s，实际 OK=%v Code=%q", CodeCoverageGap, c.OK, c.Code)
	}
}

func TestFreshness_IndexStale(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})
	for i := 0; i < testMinBarRows; i++ {
		code := mkCode(i)
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: code, ListStatus: "L", UpdatedAt: tradeDate})
		_ = rc.UpsertBar(context.Background(), mkBar(code, tradeDate))
		_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: code, TradeDate: tradeDate, Close: model.FromFloat(100)})
		_ = rc.UpsertLimit(context.Background(), model.PriceLimit{TsCode: code, TradeDate: tradeDate, UpLimit: model.FromFloat(110), DownLimit: model.FromFloat(90)})
	}
	// 不插入 index_daily → INDEX_STALE（非阻断，不使 Fresh=false）

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "IndexRows")
	if c.OK || c.Code != CodeIndexStale {
		t.Fatalf("期望 IndexRows=%s，实际 OK=%v Code=%q", CodeIndexStale, c.OK, c.Code)
	}
	// 非阻断：其余全通过时 Fresh 应为 true
	if !rep.Fresh {
		t.Fatalf("期望 IndexRows 非阻断使 Fresh=true，实际:\n%s", rep.String())
	}
}

// TestFreshness_AllPass 正向用例：八项均满足时报告 Fresh。
func TestFreshness_AllPass(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})

	idxCode := "000300.SH" // 指数码，不入候选池（仅 index_daily）
	stockCodes := make([]string, 0, testMinBarRows)
	for i := 0; i < testMinBarRows; i++ {
		stockCodes = append(stockCodes, mkCode(i))
	}
	// 指数日线（沪深300）
	_ = rc.UpsertIndexDaily(context.Background(), model.IndexDaily{TsCode: idxCode, TradeDate: tradeDate, Close: model.FromFloat(4000)})
	for _, c := range stockCodes {
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: c, ListStatus: "L", UpdatedAt: tradeDate})
		_ = rc.UpsertBar(context.Background(), mkBar(c, tradeDate))
		_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: c, TradeDate: tradeDate, Close: model.FromFloat(100)})
		_ = rc.UpsertLimit(context.Background(), model.PriceLimit{TsCode: c, TradeDate: tradeDate, UpLimit: model.FromFloat(110), DownLimit: model.FromFloat(90)})
	}

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	if !rep.Fresh {
		t.Fatalf("期望全部通过，实际:\n%s", rep.String())
	}
}

// mkCode 生成稳定的测试股票代码（避开指数代码 000300.SH）。
func mkCode(i int) string {
	return fmt.Sprintf("600%03d.SH", i)
}

// TestFreshness_CoverageSuspendedExcluded 回归：当日停牌股无日线不应产生覆盖缺口。
// 修复前：候选池含停牌股→其无日线→误判缺口→门禁永久失败→永远不出信号（历史版本核心坑）。
func TestFreshness_CoverageSuspendedExcluded(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})

	// 候选池 3 只，其中 600521 当日停牌
	cands := []string{"600519.SH", "600520.SH", "600521.SH"}
	for _, c := range cands {
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: c, ListStatus: "L", UpdatedAt: tradeDate})
	}
	_ = rc.UpsertSuspend(context.Background(), model.Suspend{TsCode: "600521.SH", TradeDate: tradeDate, SuspendType: "S", SuspendTiming: "H"})

	// 仅 600519/600520 有日线（600521 停牌无日线）
	for _, c := range []string{"600519.SH", "600520.SH"} {
		_ = rc.UpsertBar(context.Background(), mkBar(c, tradeDate))
		_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: c, TradeDate: tradeDate, Close: model.FromFloat(100)})
		_ = rc.UpsertLimit(context.Background(), model.PriceLimit{TsCode: c, TradeDate: tradeDate, UpLimit: model.FromFloat(110), DownLimit: model.FromFloat(90)})
	}

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, ok := findItem(rep, "MissingCodes")
	if !ok {
		t.Fatal("MissingCodes 检查缺失")
	}
	if !c.OK {
		t.Fatalf("期望停牌股排除后覆盖无缺口(MissingCodes PASS)，实际: %s", c.Detail)
	}
}

// TestFreshness_CoverageGapLarge 显著缺口（真·数据未同步）仍应阻断（FAIL 且 Blocking）。
func TestFreshness_CoverageGapLarge(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true, Exchange: "SSE"})

	const n = 60 // 候选 60 → 容差 = max(60/100, 50) = 50
	for i := 0; i < n; i++ {
		code := mkCode(i)
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: code, ListStatus: "L", UpdatedAt: tradeDate})
	}
	// 仅前 5 只有日线 → 缺口 55 > 容差 50（显著缺失，应阻断）
	for i := 0; i < 5; i++ {
		code := mkCode(i)
		_ = rc.UpsertBar(context.Background(), mkBar(code, tradeDate))
		_ = rc.UpsertDailyBasic(context.Background(), model.DailyBasic{TsCode: code, TradeDate: tradeDate, Close: model.FromFloat(100)})
		_ = rc.UpsertLimit(context.Background(), model.PriceLimit{TsCode: code, TradeDate: tradeDate, UpLimit: model.FromFloat(110), DownLimit: model.FromFloat(90)})
	}

	rep, err := NewFreshnessGate(st, testMinBarRows).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, ok := findItem(rep, "MissingCodes")
	if !ok {
		t.Fatal("MissingCodes 检查缺失")
	}
	if c.OK || c.Code != CodeCoverageGap {
		t.Fatalf("期望显著缺口 MissingCodes=%s(Blocking)，实际 OK=%v Code=%q", CodeCoverageGap, c.OK, c.Code)
	}
	if !c.Blocking {
		t.Fatalf("期望显著缺口为阻断项，实际 Blocking=false")
	}
}
