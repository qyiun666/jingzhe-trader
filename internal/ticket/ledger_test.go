package ticket

import (
	"context"
	"errors"
	"testing"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
)

// 测试口径：佣金 0.025%（最低 5 元）、印花税 0.1%、过户费 0.001%。
func testCost() market.CostParams {
	return market.CostParams{
		CommissionRate:  0.00025,
		MinCommission:   model.FromFloat(5),
		StampTaxRate:    0.001,
		TransferFeeRate: 0.00001,
	}
}

// openLedger 打开临时库并返回账本（本金 1 万元）。
func openLedger(t *testing.T) (*store.Store, *Ledger) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("store.Open 失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, NewLedger(s, testCost(), model.FromFloat(10000))
}

// seedTicket 直接落一张已 issued 的指令单（绕过信号链，聚焦回执记账）。
func seedTicket(t *testing.T, s *store.Store, tsCode string, dir model.Direction, qty model.Qty, price model.Fen) model.OrderTicket {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	tk := model.OrderTicket{
		TradeDate: "20260901", TsCode: tsCode, Name: "测试" + tsCode, Direction: dir,
		Qty: qty, RefPriceLow: price, RefPriceHigh: price, StopPrice: model.FromFloat(9),
		Reason: "单测种子", Urgency: "normal", Source: "test", Status: model.TicketIssued,
		ValidUntil: "2026-09-02T15:00:00+08:00", Gear: model.GearG1,
		CreatedAt: now, UpdatedAt: now,
	}
	id, err := s.TradeRepo().InsertTicket(context.Background(), tk)
	if err != nil {
		t.Fatalf("落种子指令单失败: %v", err)
	}
	tk.ID = id
	return tk
}

// seedBar 落一根日线（未复权收盘为价格口径）。
func seedBar(t *testing.T, s *store.Store, tsCode, date string, rawClose model.Fen) {
	t.Helper()
	b := model.Bar{
		TsCode: tsCode, TradeDate: date,
		Open: rawClose, High: rawClose, Low: rawClose, Close: rawClose, PreClose: rawClose,
		RawClose: rawClose, AdjFactor: 1.0,
	}
	if err := s.MarketRepo().UpsertBar(context.Background(), b); err != nil {
		t.Fatalf("落种子日线失败: %v", err)
	}
}

// TestTicketRequiredFields 对应验收 #9：7 个必填字段缺失即拒绝生成。
func TestTicketRequiredFields(t *testing.T) {
	s, _ := openLedger(t)
	svc := NewService(s)
	p := risk.Resolve(risk.DefaultBase(model.FromFloat(10000)), model.GearG1, false, risk.NoPace{})
	days := []string{"20260901", "20260902"}
	good := model.Signal{
		TradeDate: "20260901", TsCode: "sh600519", Name: "贵州茅台", Direction: model.DirBuy,
		Rule: "buy_trend", Confidence: 0.8, RefPrice: model.FromFloat(50), Reason: "趋势确认",
	}
	tk, err := svc.Create(context.Background(), good, 100, p, model.GearG1, days)
	if err != nil {
		t.Fatalf("合法指令单生成失败: %v", err)
	}
	// 7 必填字段逐一非空断言
	if tk.TradeDate == "" || tk.TsCode == "" || tk.Name == "" || !tk.Direction.Valid() ||
		tk.Qty <= 0 || tk.Reason == "" || tk.ValidUntil == "" {
		t.Fatalf("必填字段存在空值: %+v", tk)
	}
	if tk.Status != model.TicketDrafted {
		t.Errorf("新单状态=%s, 期望 drafted", tk.Status)
	}
	// 有效期 = 下一交易日 15:00
	vu, err := time.Parse(time.RFC3339, tk.ValidUntil)
	if err != nil {
		t.Fatalf("valid_until 解析失败: %v", err)
	}
	want := time.Date(2026, 9, 2, 15, 0, 0, 0, market.Loc)
	if !vu.Equal(want) {
		t.Errorf("valid_until=%s, 期望 %s（下一交易日 15:00）", tk.ValidUntil, want)
	}
	// 缺字段逐项拒绝（qty 由 Create 的独立参数传入，另设 qty<=0 用例）
	bad := []model.Signal{
		{Name: "", TsCode: "sh600519", TradeDate: "20260901", Direction: model.DirBuy, RefPrice: model.FromFloat(50), Reason: "r"},
		{Name: "x", TsCode: "", TradeDate: "20260901", Direction: model.DirBuy, RefPrice: model.FromFloat(50), Reason: "r"},
		{Name: "x", TsCode: "sh600519", TradeDate: "", Direction: model.DirBuy, RefPrice: model.FromFloat(50), Reason: "r"},
		{Name: "x", TsCode: "sh600519", TradeDate: "20260901", Direction: "hold", RefPrice: model.FromFloat(50), Reason: "r"},
		{Name: "x", TsCode: "sh600519", TradeDate: "20260901", Direction: model.DirBuy, RefPrice: model.FromFloat(50), Reason: ""},
	}
	for i, sig := range bad {
		if _, err := svc.Create(context.Background(), sig, 100, p, model.GearG1, days); !errors.Is(err, ErrRequiredField) {
			t.Errorf("缺字段用例 %d 应返回 ErrRequiredField, 实际: %v", i, err)
		}
	}
	// qty <= 0 拒绝
	if _, err := svc.Create(context.Background(), good, 0, p, model.GearG1, days); !errors.Is(err, ErrRequiredField) {
		t.Errorf("qty=0 应返回 ErrRequiredField, 实际: %v", err)
	}
}

// TestReportFillIdempotent 对应验收 #10：同一 ticket_id 连续回执两次，fill 一行、position 只加一次。
func TestReportFillIdempotent(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	tk := seedTicket(t, s, "sh600001", model.DirBuy, 900, model.FromFloat(10))

	req := FillRequest{TicketID: tk.ID, TsCode: tk.TsCode, Qty: 900, Price: model.FromFloat(10), Actor: "test"}
	res1, err := led.ReportFill(ctx, req)
	if err != nil {
		t.Fatalf("首次回执失败: %v", err)
	}
	if res1.Duplicate {
		t.Errorf("首次回执不应为重复")
	}
	res2, err := led.ReportFill(ctx, req)
	if err != nil {
		t.Fatalf("重复回执应幂等命中而非报错: %v", err)
	}
	if !res2.Duplicate {
		t.Errorf("第二次回执应标记 Duplicate")
	}
	fills, err := s.TradeRepo().CountFills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fills != 1 {
		t.Errorf("fill 行数=%d, 期望 1（回执幂等）", fills)
	}
	pos, err := s.TradeRepo().GetPosition(ctx, tk.TsCode)
	if err != nil {
		t.Fatal(err)
	}
	if pos.TotalQty != 900 {
		t.Errorf("持仓=%d, 期望 900（position 只加一次）", int64(pos.TotalQty))
	}
	// T+1：当日买入 available_qty 不增
	if pos.AvailableQty != 0 {
		t.Errorf("当日买入 available_qty=%d, 期望 0", int64(pos.AvailableQty))
	}
	if pos.TodayBought != 900 {
		t.Errorf("today_bought=%d, 期望 900", int64(pos.TodayBought))
	}
	// 现金推算：10000 元本金 − 900 股×10 元 − 费用
	cash, err := led.Cash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	amount := model.FromFloat(9000)
	tc := market.CalcTradeCost(amount, true, 0.00025, 0.001, 0.00001, model.FromFloat(5))
	if want := model.FromFloat(10000) - tc.TotalCost; cash != want {
		t.Errorf("现金=%s, 期望 %s", cash, want)
	}
}

// TestReportFillFailureRollsBack 对应验收 #11：写库失败 → ReportFill 返回 error，position 与现金不变。
func TestReportFillFailureRollsBack(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	tk := seedTicket(t, s, "sh600002", model.DirBuy, 500, model.FromFloat(20))

	// 先建立基线持仓与回执
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tk.ID, Qty: 500, Price: model.FromFloat(20)}); err != nil {
		t.Fatalf("基线回执失败: %v", err)
	}
	posBefore, err := s.TradeRepo().GetPosition(ctx, tk.TsCode)
	if err != nil {
		t.Fatal(err)
	}
	cashBefore, err := led.Cash(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// 制造写库失败：用 RAISE(ABORT) 触发器让 fill 的 INSERT 必然失败（表结构保持完好，便于事后对账）
	tk2 := seedTicket(t, s, "sh600003", model.DirBuy, 300, model.FromFloat(15))
	if _, err := s.WriteDB().Exec(
		`CREATE TRIGGER fail_fill_insert BEFORE INSERT ON fill BEGIN SELECT RAISE(ABORT, 'simulated db failure'); END`); err != nil {
		t.Fatalf("模拟写库失败环境失败: %v", err)
	}
	_, err = led.ReportFill(ctx, FillRequest{TicketID: tk2.ID, Qty: 300, Price: model.FromFloat(15)})
	if err == nil {
		t.Fatalf("写库失败应上抛 error")
	}
	if _, err := s.WriteDB().Exec("DROP TRIGGER fail_fill_insert"); err != nil {
		t.Fatalf("清理触发器失败: %v", err)
	}

	// 账本未被改动：fill 行数不变、持仓与现金不变
	fillsAfter, err := s.TradeRepo().CountFills(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fillsAfter != 1 {
		t.Errorf("fill 行数=%d, 期望 1（失败回执零写入）", fillsAfter)
	}
	posAfter, err := s.TradeRepo().GetPosition(ctx, tk.TsCode)
	if err != nil {
		t.Fatal(err)
	}
	if posAfter != posBefore {
		t.Errorf("position 被意外改动: before=%+v after=%+v", posBefore, posAfter)
	}
	cashAfter, err := led.Cash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cashAfter != cashBefore {
		t.Errorf("cash 被意外改动: before=%s after=%s", cashBefore, cashAfter)
	}
	if _, err := s.TradeRepo().GetPosition(ctx, tk2.TsCode); err == nil {
		t.Errorf("失败回执不应新建持仓")
	}
}

// TestSettleT1 对应验收 #12：settle 后 available_qty 增加、today_bought 归零。
func TestSettleT1(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	tk := seedTicket(t, s, "sh600004", model.DirBuy, 800, model.FromFloat(12.5))
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tk.ID, Qty: 800, Price: model.FromFloat(12.5)}); err != nil {
		t.Fatalf("回执失败: %v", err)
	}
	pos, _ := s.TradeRepo().GetPosition(ctx, tk.TsCode)
	if pos.AvailableQty != 0 || pos.TodayBought != 800 {
		t.Fatalf("结转前 available=%d today=%d, 期望 0/800", int64(pos.AvailableQty), int64(pos.TodayBought))
	}
	n, err := led.SettleT1(ctx, "20260902")
	if err != nil {
		t.Fatalf("T+1 结转失败: %v", err)
	}
	if n != 1 {
		t.Errorf("结转行数=%d, 期望 1", n)
	}
	pos, err = s.TradeRepo().GetPosition(ctx, tk.TsCode)
	if err != nil {
		t.Fatal(err)
	}
	if pos.AvailableQty != 800 {
		t.Errorf("结转后 available_qty=%d, 期望 800", int64(pos.AvailableQty))
	}
	if pos.TodayBought != 0 {
		t.Errorf("结转后 today_bought=%d, 期望 0", int64(pos.TodayBought))
	}
}

// TestSellFill 减仓回执：总仓与可卖同步减少，成本价不变。
func TestSellFill(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	// 先建仓：1000 股 @10 元
	buy := seedTicket(t, s, "sh600005", model.DirBuy, 1000, model.FromFloat(10))
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: buy.ID, Qty: 1000, Price: model.FromFloat(10)}); err != nil {
		t.Fatal(err)
	}
	if _, err := led.SettleT1(ctx, "20260902"); err != nil {
		t.Fatal(err)
	}
	// 卖出 400 股 @12 元
	sell := seedTicket(t, s, "sh600005", model.DirSell, 400, model.FromFloat(12))
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: sell.ID, Qty: 400, Price: model.FromFloat(12)}); err != nil {
		t.Fatalf("卖出回执失败: %v", err)
	}
	pos, _ := s.TradeRepo().GetPosition(ctx, "sh600005")
	if pos.TotalQty != 600 || pos.AvailableQty != 600 {
		t.Errorf("卖出后 total=%d available=%d, 期望 600/600", int64(pos.TotalQty), int64(pos.AvailableQty))
	}
	if pos.CostPrice != model.FromFloat(10) {
		t.Errorf("卖出不减权成本: cost=%s, 期望 10.00", pos.CostPrice)
	}
	// 超卖拒绝
	bad := seedTicket(t, s, "sh600005", model.DirSell, 700, model.FromFloat(12))
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: bad.ID, Qty: 700, Price: model.FromFloat(12)}); err == nil {
		t.Errorf("超卖应报错")
	}
}

// TestSnapshotMarketValue 对应验收 #13：market_value 现算；停牌股取停牌前收盘。
func TestSnapshotMarketValue(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	// 甲股：当日（20260901）有行情，收盘 11 元，持仓 1000 股 → 11000 元
	tkA := seedTicket(t, s, "sh600006", model.DirBuy, 1000, model.FromFloat(10))
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tkA.ID, Qty: 1000, Price: model.FromFloat(10)}); err != nil {
		t.Fatal(err)
	}
	seedBar(t, s, "sh600006", "20260901", model.FromFloat(11))
	// 乙股：停牌（最后行情 20260829），收盘 5 元，持仓 2000 股 → 10000 元
	tkB := seedTicket(t, s, "sz000007", model.DirBuy, 2000, model.FromFloat(5))
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tkB.ID, Qty: 2000, Price: model.FromFloat(5)}); err != nil {
		t.Fatal(err)
	}
	seedBar(t, s, "sz000007", "20260829", model.FromFloat(5))

	sn, err := led.TakeSnapshot(ctx, "20260901", model.GearG1, false)
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	if want := model.FromFloat(11000) + model.FromFloat(10000); sn.MarketValue != want {
		t.Errorf("market_value=%s, 期望 %s（停牌股取停牌前收盘）", sn.MarketValue, want)
	}
	if sn.PositionCount != 2 {
		t.Errorf("position_count=%d, 期望 2", sn.PositionCount)
	}
	if sn.TotalAsset != sn.Cash+sn.MarketValue {
		t.Errorf("total_asset=%s ≠ cash+mv", sn.TotalAsset)
	}
}

// TestInitialCapitalWriteOnce 对应验收 #14：第二次 sync_portfolio 不覆盖本金，有 action_log 拒绝记录。
func TestInitialCapitalWriteOnce(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	items := []PortfolioInput{{TsCode: "sh600008", TotalQty: 100, AvailableQty: 100, CostPrice: model.FromFloat(10)}}
	n, rejected, err := led.SyncPortfolio(ctx, "20260901", model.FromFloat(10000), items, "test")
	if err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	if n != 1 || rejected {
		t.Errorf("首次同步 n=%d rejected=%v, 期望 1/false", n, rejected)
	}
	// 第二次：本金不同 → 拒绝覆盖 + 审计
	n, rejected, err = led.SyncPortfolio(ctx, "20260902", model.FromFloat(20000), items, "test")
	if err != nil {
		t.Fatalf("二次同步失败: %v", err)
	}
	if !rejected {
		t.Errorf("二次同步应拒绝覆盖本金")
	}
	if n != 1 {
		t.Errorf("二次同步持仓仍应校准: n=%d", n)
	}
	// 审计存在
	var cnt int
	if err := s.ReadDB().Get(&cnt,
		"SELECT COUNT(*) FROM action_log WHERE object_id='initial_capital' AND action='rejected_overwrite'"); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("拒绝审计日志=%d 条, 期望 1", cnt)
	}
	// 本金未被覆盖：config_kv 中仍是首次值
	row, err := s.ConfigRepo().Get(ctx, "account.initial_capital")
	if err != nil {
		t.Fatal(err)
	}
	if row.Value != "1000000" {
		t.Errorf("本金被覆盖为 %s, 期望 1000000", row.Value)
	}
}

// TestTransitionIllegal 非法状态转移报错并落审计。
func TestTransitionIllegal(t *testing.T) {
	s, _ := openLedger(t)
	ctx := context.Background()
	tk := seedTicket(t, s, "sh600009", model.DirBuy, 100, model.FromFloat(9))
	svc := NewService(s)
	// issued → drafted 非法
	if err := svc.Transition(ctx, tk.ID, model.TicketDrafted, "test", "非法转移测试"); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("非法转移应返回 ErrIllegalTransition, 实际: %v", err)
	}
	// 合法转移 issued → skipped 落审计
	if err := svc.Transition(ctx, tk.ID, model.TicketSkipped, "test", "人工放弃"); err != nil {
		t.Fatalf("合法转移失败: %v", err)
	}
	t2, err := s.TradeRepo().GetTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Status != model.TicketSkipped || t2.ClosedAt == "" {
		t.Errorf("终态流转未生效: %+v", t2)
	}
	var cnt int
	if err := s.ReadDB().Get(&cnt, "SELECT COUNT(*) FROM action_log WHERE object_type='order_ticket' AND object_id=?",
		tk.ID); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("流转审计=%d 条, 期望 1", cnt)
	}
}
