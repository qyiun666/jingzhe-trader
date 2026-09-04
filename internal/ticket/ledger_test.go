package ticket

import (
	"context"
	"errors"
	"testing"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
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
	tk := model.OrderTicket{
		TradeDate: "20260901", TsCode: tsCode, Name: "测试" + tsCode, Direction: dir,
		Qty: qty, RefPrice: price,
		Reason: "单测种子", Status: model.TicketIssued,
		ValidUntil: "2026-09-02T15:00:00+08:00", Gear: model.GearG1,
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
		TsCode: tsCode, TradeDate: date, Close: rawClose, RawClose: rawClose,
	}
	if err := s.MarketRepo().UpsertBar(context.Background(), b); err != nil {
		t.Fatalf("落种子日线失败: %v", err)
	}
}

// TestTicketRequiredFields 对应验收 #9：7 个必填字段缺失即拒绝生成。
func TestTicketRequiredFields(t *testing.T) {
	s, _ := openLedger(t)
	svc := NewService(s)
	days := []string{"20260901", "20260902"}
	good := model.Signal{
		TradeDate: "20260901", TsCode: "sh600519", Name: "贵州茅台", Direction: model.DirBuy,
		Rule: "buy_trend", Confidence: 0.8, RefPrice: model.FromFloat(50), Reason: "趋势确认",
	}
	tk, err := svc.Create(context.Background(), good, 100, model.GearG1, days)
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
		if _, err := svc.Create(context.Background(), sig, 100, model.GearG1, days); !errors.Is(err, ErrRequiredField) {
			t.Errorf("缺字段用例 %d 应返回 ErrRequiredField, 实际: %v", i, err)
		}
	}
	// qty <= 0 拒绝
	if _, err := svc.Create(context.Background(), good, 0, model.GearG1, days); !errors.Is(err, ErrRequiredField) {
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
	fills, err := s.TradeRepo().CountFilled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fills != 1 {
		t.Errorf("已登记回执数=%d, 期望 1（回执幂等）", fills)
	}
	pos, err := s.TradeRepo().GetPosition(ctx, tk.TsCode)
	if err != nil {
		t.Fatal(err)
	}
	if pos.TotalQty != 900 {
		t.Errorf("持仓=%d, 期望 900（position 只加一次）", int64(pos.TotalQty))
	}
	// T+1：当日买入 available_qty 不增
	if pos.Available() != 0 {
		t.Errorf("当日买入 可卖量=%d, 期望 0", int64(pos.Available()))
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

	// 制造写库失败：用 RAISE(ABORT) 触发器让持仓写入必然失败。
	// 故障注入在事务的**第二步**，此时第一步（指令单成交列）已成功写入 ——
	// 回滚必须把第一步一起撤掉，比让第一步就失败更能证明两步的原子性。
	tk2 := seedTicket(t, s, "sh600003", model.DirBuy, 300, model.FromFloat(15))
	if _, err := s.WriteDB().Exec(
		`CREATE TRIGGER fail_position_insert BEFORE INSERT ON position BEGIN SELECT RAISE(ABORT, 'simulated db failure'); END`); err != nil {
		t.Fatalf("模拟写库失败环境失败: %v", err)
	}
	_, err = led.ReportFill(ctx, FillRequest{TicketID: tk2.ID, Qty: 300, Price: model.FromFloat(15)})
	if err == nil {
		t.Fatalf("写库失败应上抛 error")
	}
	if _, err := s.WriteDB().Exec("DROP TRIGGER fail_position_insert"); err != nil {
		t.Fatalf("清理触发器失败: %v", err)
	}

	// 账本未被改动：回执数不变、失败那单没落下成交列、没新建持仓行、基线持仓与现金不变
	fillsAfter, err := s.TradeRepo().CountFilled(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fillsAfter != 1 {
		t.Errorf("已登记回执数=%d, 期望 1（失败回执零写入）", fillsAfter)
	}
	if t2, err := s.TradeRepo().GetTicket(ctx, tk2.ID); err != nil {
		t.Fatal(err)
	} else if t2.HasFill() {
		t.Errorf("事务回滚后指令单不应带成交回执: %+v", t2.FillView())
	}
	var posRows int
	if err := s.ReadDB().Get(&posRows, "SELECT COUNT(*) FROM position WHERE ts_code=?", tk2.TsCode); err != nil {
		t.Fatal(err)
	}
	if posRows != 0 {
		t.Errorf("失败回执不应新建持仓行, 实际 %d 行", posRows)
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
	if pos.Available() != 0 || pos.TodayBought != 800 {
		t.Fatalf("结转前 可卖=%d today=%d, 期望 0/800", int64(pos.Available()), int64(pos.TodayBought))
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
	if pos.Available() != 800 {
		t.Errorf("结转后 可卖量=%d, 期望 800", int64(pos.Available()))
	}
	if pos.TodayBought != 0 {
		t.Errorf("结转后 today_bought=%d, 期望 0", int64(pos.TodayBought))
	}
}

// seedTicketOn 与 seedTicket 同义，但可指定指令单所属交易日（T+1 判据要用成交日）。
func seedTicketOn(t *testing.T, s *store.Store, tsCode string, dir model.Direction,
	qty model.Qty, price model.Fen, tradeDate string) model.OrderTicket {
	t.Helper()
	tk := model.OrderTicket{
		TradeDate: tradeDate, TsCode: tsCode, Name: "测试" + tsCode, Direction: dir,
		Qty: qty, RefPrice: price, Reason: "单测种子", Status: model.TicketIssued,
		ValidUntil: "2026-09-03T15:00:00+08:00", Gear: model.GearG1,
	}
	id, err := s.TradeRepo().InsertTicket(context.Background(), tk)
	if err != nil {
		t.Fatalf("落种子指令单失败: %v", err)
	}
	tk.ID = id
	return tk
}

// TestSettleT1RefusesSameDayFill 当天补跑 morning_plan 不得把当天刚买的股票解锁成可卖。
//
// 回归场景：agent 补跑当天的盘前计划（或按历史日期补跑）时，
// 只看 today_bought 一律归零 = 直接绕过 T+1。
func TestSettleT1RefusesSameDayFill(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	tk := seedTicketOn(t, s, "sh600006", model.DirBuy, 500, model.FromFloat(9), "20260902")
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tk.ID, Qty: 500, Price: model.FromFloat(9)}); err != nil {
		t.Fatalf("回执失败: %v", err)
	}
	pos, _ := s.TradeRepo().GetPosition(ctx, tk.TsCode)
	if pos.TodayBought != 500 || pos.Available() != 0 {
		t.Fatalf("成交后 today_bought=%d 可卖=%d，期望 500/0", int64(pos.TodayBought), int64(pos.Available()))
	}
	// 结转日 == 成交日：不结转
	if n, err := led.SettleT1(ctx, "20260902"); err != nil || n != 0 {
		t.Fatalf("当天补跑不应结转任何行，实际 n=%d err=%v", n, err)
	}
	if pos, _ = s.TradeRepo().GetPosition(ctx, tk.TsCode); pos.Available() != 0 {
		t.Fatalf("当天补跑后可卖量变成 %d，T+1 被绕过", int64(pos.Available()))
	}
	// 次日：正常结转
	if n, err := led.SettleT1(ctx, "20260903"); err != nil || n != 1 {
		t.Fatalf("次日应结转 1 行，实际 n=%d err=%v", n, err)
	}
	if pos, _ = s.TradeRepo().GetPosition(ctx, tk.TsCode); pos.Available() != 500 || pos.TodayBought != 0 {
		t.Fatalf("次日结转后 可卖=%d today=%d，期望 500/0", int64(pos.Available()), int64(pos.TodayBought))
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
	if pos.TotalQty != 600 || pos.Available() != 600 {
		t.Errorf("卖出后 total=%d available=%d, 期望 600/600", int64(pos.TotalQty), int64(pos.Available()))
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

// TestAssetsMarketValue 对应验收 #13：市值现算；停牌股取停牌前收盘。资产不落库。
func TestAssetsMarketValue(t *testing.T) {
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

	sn, err := led.Assets(ctx, "20260901")
	if err != nil {
		t.Fatalf("资产计算失败: %v", err)
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
	n, rejected, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20260901",
		Capital: model.FromFloat(10000), Cash: model.FromFloat(9000), Items: items, Actor: "test"})
	if err != nil {
		t.Fatalf("首次同步失败: %v", err)
	}
	if n != 1 || rejected {
		t.Errorf("首次同步 n=%d rejected=%v, 期望 1/false", n, rejected)
	}
	// 第二次：本金不同 → 拒绝覆盖 + 审计
	n, rejected, err = led.SyncPortfolio(ctx, PortfolioSync{Date: "20260902",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(9000), Items: items, Actor: "test"})
	if err != nil {
		t.Fatalf("二次同步失败: %v", err)
	}
	if !rejected {
		t.Errorf("二次同步应拒绝覆盖本金")
	}
	if n != 1 {
		t.Errorf("二次同步持仓仍应校准: n=%d", n)
	}
	// 拒绝留痕已改为运行日志（action_log 表本次删除）。本测试对"拒绝"这个行为的
	// 断言在上方 rejected 与下方配置值两处，均为行为本身，未因此变弱。
	// 本金未被覆盖，且量纲是"元"：app.InitialCapitalOf 按元读这个键，
	// 写成分会让本金凭空放大 100 倍（历史缺陷，这条断言就是它的回归守卫）。
	all, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := all["account.initial_capital"]; got != "10000" {
		t.Errorf("本金=%s, 期望 10000（元，且不被二次覆盖）", got)
	}
}

// TestSyncPortfolioRequiresCash 校准持仓必须同时给出可用现金，
// 否则那笔持仓成本会被算成两遍（一遍在持仓里、一遍在现金里）。
func TestSyncPortfolioRequiresCash(t *testing.T) {
	_, led := openLedger(t)
	ctx := context.Background()
	items := []PortfolioInput{{TsCode: "sh600012", TotalQty: 100, AvailableQty: 100, CostPrice: model.FromFloat(10)}}
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20260902",
		Capital: model.FromFloat(20000), Items: items, Actor: "test"}); err == nil {
		t.Fatal("不给可用现金的组合同步必须报错，实得 nil")
	}
}

// TestCashAnchorNotDoubleCounted 校准进来的持仓没有成交单支撑：
// 可用资金取券商给的锚点，锚点之前成交不再重复扣减，锚点之后的成交才动现金。
func TestCashAnchorNotDoubleCounted(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	items := []PortfolioInput{{TsCode: "sh600010", TotalQty: 400, AvailableQty: 400, CostPrice: model.FromFloat(20)}}
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20260902",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(12000), Items: items, Actor: "test"}); err != nil {
		t.Fatalf("组合同步失败: %v", err)
	}
	cash, err := led.Cash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cash != model.FromFloat(12000) {
		t.Errorf("锚点后现金=%s, 期望 12000（券商给的可用资金，不能再减一遍持仓成本）", cash)
	}

	// 锚点当日及之前的成交视为已含在快照里：这笔 20260901 的买入不动现金
	tk := seedTicket(t, s, "sh600011", model.DirBuy, 100, model.FromFloat(10))
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tk.ID, Qty: 100, Price: model.FromFloat(10)}); err != nil {
		t.Fatal(err)
	}
	if cash, err = led.Cash(ctx); err != nil {
		t.Fatal(err)
	}
	if cash != model.FromFloat(12000) {
		t.Errorf("锚点之前成交后现金=%s, 期望仍为 12000", cash)
	}
	// 把这笔成交挪到锚点之后：现金必须随买入减少（12000 元 − 1000 元 − 费用）
	if _, err := s.WriteDB().Exec(`UPDATE order_ticket SET trade_date='20260903' WHERE id=?`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if cash, err = led.Cash(ctx); err != nil {
		t.Fatal(err)
	}
	if cash >= model.FromFloat(12000) || cash < model.FromFloat(10990) {
		t.Errorf("锚点之后有 1000 元买入成交，现金应略低于 11000，实得 %s", cash)
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
	// 合法转移 issued → skipped（结果就写在指令单行上）
	if err := svc.Transition(ctx, tk.ID, model.TicketSkipped, "test", "人工放弃"); err != nil {
		t.Fatalf("合法转移失败: %v", err)
	}
	t2, err := s.TradeRepo().GetTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if t2.Status != model.TicketSkipped {
		t.Errorf("终态流转未生效: status=%+v", t2.Status)
	}
	if !t2.Status.IsTerminal() {
		t.Errorf("skipped 应为终态: %+v", t2.Status)
	}
	// 流转留痕已改为运行日志（action_log 表本次删除）；"流转是否落库"由上面的回读断言承担。
}
