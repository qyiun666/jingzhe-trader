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
		Close: model.FromFloat(100), VolLot: 0, RawClose: model.FromFloat(100),
	}
}

func TestFreshness_CalMissing(t *testing.T) {
	st := openTestStore(t)
	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), "20260901")
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
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: "20260901", IsOpen: false})

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), "20260901")
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
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true})

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), tradeDate)
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
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true})
	// 仅 1 行（< 阈值 3）
	_ = rc.UpsertBar(context.Background(), mkBar("600519.SH", tradeDate))

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), tradeDate)
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
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true})
	_ = rc.UpsertBar(context.Background(), mkBar("600519.SH", tradeDate))
	_ = rc.UpsertBar(context.Background(), mkBar("600520.SH", tradeDate))
	_ = rc.UpsertBar(context.Background(), mkBar("600521.SH", tradeDate))
	// 估值截面仅 1 行（< 阈值 3）；截面盖在已有的 stock_basic 行上，不另建行
	_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: "600519.SH", ListStatus: "L"})
	_ = rc.SaveValuation(context.Background(), tradeDate, []model.Valuation{{TsCode: "600519.SH"}})

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "BasicRows")
	if c.OK || c.Code != CodeBasicRowsLow {
		t.Fatalf("期望 BasicRows=%s，实际 OK=%v Code=%q", CodeBasicRowsLow, c.OK, c.Code)
	}
}

func TestFreshness_CoverageGap(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true})
	// 候选池 3 只（list_status='L'），但仅 2 只有日线 → 覆盖缺口
	cands := []string{"600519.SH", "600520.SH", "600521.SH"}
	for _, c := range cands {
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: c, ListStatus: "L"})
	}
	_ = rc.UpsertBar(context.Background(), mkBar("600519.SH", tradeDate))
	_ = rc.UpsertBar(context.Background(), mkBar("600520.SH", tradeDate))
	_ = rc.SaveValuation(context.Background(), tradeDate, []model.Valuation{
		{TsCode: "600519.SH"}, {TsCode: "600520.SH"}})

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "MissingCodes")
	if c.OK || c.Code != CodeCoverageGap {
		t.Fatalf("期望 MissingCodes=%s，实际 OK=%v Code=%q", CodeCoverageGap, c.OK, c.Code)
	}
}

// TestFreshness_IndexStale 大盘指数日线缺失是阻断项：
// 买入闸门（跌破 MA20 关漏斗）与卖出规则（大盘恶化）都读这一根，缺了就不能出指令。
func TestFreshness_IndexStale(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	ctx := context.Background()
	tradeDate := "20260901"
	_ = rc.UpsertCal(ctx, store.CalRow{CalDate: tradeDate, IsOpen: true})
	for i := 0; i < testMinBarRows; i++ {
		code := mkCode(i)
		_ = rc.UpsertStockBasic(ctx, model.StockBasic{TsCode: code, ListStatus: "L"})
		_ = rc.UpsertBar(ctx, mkBar(code, tradeDate))
		_ = rc.SaveValuation(ctx, tradeDate, []model.Valuation{{TsCode: code}})
	}
	// 故意不写指数日线

	g := NewFreshnessGate(st, testMinBarRows, 0)
	rep, err := g.Check(ctx, tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	c, _ := findItem(rep, "IndexRows")
	if c.OK || c.Code != CodeIndexStale || !c.Blocking {
		t.Fatalf("期望 IndexRows=%s 且 Blocking=true，实际 OK=%v Code=%q Blocking=%v",
			CodeIndexStale, c.OK, c.Code, c.Blocking)
	}
	if rep.Fresh {
		t.Fatalf("指数缺失应使门禁整体不新鲜，实际:\n%s", rep.String())
	}

	// 补上指数日线后同一份数据应当放行（证明拦的确实是指数这一项）
	_ = rc.UpsertBar(ctx, model.Bar{TsCode: store.MarketIndex, TradeDate: tradeDate,
		Close: model.FromFloat(4000), RawClose: model.FromFloat(4000)})
	rep2, err := g.Check(ctx, tradeDate)
	if err != nil {
		t.Fatalf("补指数后 Check 失败: %v", err)
	}
	if !rep2.Fresh {
		t.Fatalf("补上 %s 日线后应放行，实际:\n%s", store.MarketIndex, rep2.String())
	}
}

// TestFreshness_AllPass 正向用例：八项均满足时报告 Fresh。
func TestFreshness_AllPass(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true})

	idxCode := "000300.SH" // 指数码，不入候选池（仅 index_daily）
	stockCodes := make([]string, 0, testMinBarRows)
	for i := 0; i < testMinBarRows; i++ {
		stockCodes = append(stockCodes, mkCode(i))
	}
	// 指数日线（沪深300）
	_ = rc.UpsertBar(context.Background(), model.Bar{TsCode: idxCode, TradeDate: tradeDate, Close: model.FromFloat(4000), VolLot: 0, RawClose: 0})
	for _, c := range stockCodes {
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: c, ListStatus: "L"})
		_ = rc.UpsertBar(context.Background(), mkBar(c, tradeDate))
		_ = rc.SaveValuation(context.Background(), tradeDate, []model.Valuation{{TsCode: c}})
	}

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), tradeDate)
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
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true})

	// 候选池 3 只，其中 600521 当日停牌
	cands := []string{"600519.SH", "600520.SH", "600521.SH"}
	for _, c := range cands {
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: c, ListStatus: "L"})
	}
	_ = rc.SaveSuspended(context.Background(), tradeDate, []string{"600521.SH"})

	// 仅 600519/600520 有日线（600521 停牌无日线）
	for _, c := range []string{"600519.SH", "600520.SH"} {
		_ = rc.UpsertBar(context.Background(), mkBar(c, tradeDate))
		_ = rc.SaveValuation(context.Background(), tradeDate, []model.Valuation{{TsCode: c}})
	}

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), tradeDate)
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
	_ = rc.UpsertCal(context.Background(), store.CalRow{CalDate: tradeDate, IsOpen: true})

	const n = 60 // 候选 60 → 容差 = max(60/100, 50) = 50
	for i := 0; i < n; i++ {
		code := mkCode(i)
		_ = rc.UpsertStockBasic(context.Background(), model.StockBasic{TsCode: code, ListStatus: "L"})
	}
	// 仅前 5 只有日线 → 缺口 55 > 容差 50（显著缺失，应阻断）
	for i := 0; i < 5; i++ {
		code := mkCode(i)
		_ = rc.UpsertBar(context.Background(), mkBar(code, tradeDate))
		_ = rc.SaveValuation(context.Background(), tradeDate, []model.Valuation{{TsCode: code}})
	}

	rep, err := NewFreshnessGate(st, testMinBarRows, 0).Check(context.Background(), tradeDate)
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

// TestFreshness_WindowShort 回归：当日行数达标但因子窗口缺交易日，必须阻断。
// 修复前：只查当日行数，窗口缺几天时动量/MA20 会拿 35 年前的残留行凑数，算出跨十年的"涨幅"。
func TestFreshness_WindowShort(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	codes := []string{mkCode(1), mkCode(2), mkCode(3)}
	for _, c := range codes {
		_ = rc.UpsertStockBasic(ctx, model.StockBasic{TsCode: c, ListStatus: "L"})
	}
	// 日历给 5 个开市日，日线只写最后一天
	dates := []string{"20260827", "20260828", "20260831", "20260901"}
	for _, d := range dates {
		_ = rc.UpsertCal(ctx, store.CalRow{CalDate: d, IsOpen: true})
		for _, c := range codes {
			_ = rc.SaveValuation(ctx, d, []model.Valuation{{TsCode: c}})
		}
	}
	for _, c := range codes {
		_ = rc.UpsertBar(ctx, mkBar(c, tradeDate))
	}
	rep, err := NewFreshnessGate(st, testMinBarRows, 4).Check(ctx, tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	item, ok := findItem(rep, "WindowOK")
	if !ok {
		t.Fatalf("缺少 WindowOK 检查项:\n%s", rep.String())
	}
	if item.Code != CodeWindowShort {
		t.Errorf("WindowOK 失败码 = %q，期望 %q", item.Code, CodeWindowShort)
	}
	if rep.Fresh {
		t.Errorf("窗口缺口应使门禁不新鲜:\n%s", rep.String())
	}
}

// TestFreshness_WindowComplete 窗口齐全时 WindowOK 通过。
func TestFreshness_WindowComplete(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"
	codes := []string{mkCode(1), mkCode(2), mkCode(3)}
	for _, c := range codes {
		_ = rc.UpsertStockBasic(ctx, model.StockBasic{TsCode: c, ListStatus: "L"})
	}
	for _, d := range []string{"20260826", "20260827", "20260828", "20260831", "20260901"} {
		_ = rc.UpsertCal(ctx, store.CalRow{CalDate: d, IsOpen: true})
		for _, c := range codes {
			_ = rc.UpsertBar(ctx, mkBar(c, d))
			_ = rc.SaveValuation(ctx, d, []model.Valuation{{TsCode: c}})
		}
	}
	rep, err := NewFreshnessGate(st, testMinBarRows, 5).Check(ctx, tradeDate)
	if err != nil {
		t.Fatalf("Check 失败: %v", err)
	}
	item, _ := findItem(rep, "WindowOK")
	if !item.OK {
		t.Errorf("窗口齐全时 WindowOK 应通过: %s", item.Detail)
	}
}
