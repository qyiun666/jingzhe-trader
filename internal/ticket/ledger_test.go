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
		ValidUntil: "2026-09-02T15:00:00+08:00",
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
	tk, err := svc.Create(context.Background(), good, 100, days)
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
		if _, err := svc.Create(context.Background(), sig, 100, days); !errors.Is(err, ErrRequiredField) {
			t.Errorf("缺字段用例 %d 应返回 ErrRequiredField, 实际: %v", i, err)
		}
	}
	// qty <= 0 拒绝
	if _, err := svc.Create(context.Background(), good, 0, days); !errors.Is(err, ErrRequiredField) {
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
		ValidUntil: "2026-09-03T15:00:00+08:00",
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

// TestSyncPortfolioKeepsFirstOpenDate 生产事故回归（agent_issues #12-B）：
// 券商口径的持仓同步不带建仓日，整列覆盖把既有建仓日擦成空 →
// signal 算不出持有天数（-1）→ 最小持有期豁免失效 → 每天生成淘汰刚买入持仓的卖出单。
// 20261015 11:00 与 11:24 两次 sync_portfolio(positions=2) 正是把 600704/601766 擦掉的现场，
// 这里用同一序列复现：建仓 → 同步校准 → 建仓日必须原地不动。
func TestSyncPortfolioKeepsFirstOpenDate(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	code := "sh600704"

	tk := seedTicketOn(t, s, code, model.DirBuy, 200, model.FromFloat(5.05), "20261012")
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tk.ID, Qty: 200, Price: model.FromFloat(5.05)}); err != nil {
		t.Fatalf("建仓回执失败: %v", err)
	}
	if got := mustPos(t, s, ctx, code).FirstOpenDate; got != "20261012" {
		t.Fatalf("建仓后 first_open_date=%q, 期望 20261012", got)
	}

	// 10-15 的组合同步：不带建仓日（券商 App 持仓页没有这一列），只校准数量与成本
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261015",
		Capital: model.FromFloat(0), Cash: model.FromFloat(10163),
		Items: []PortfolioInput{{TsCode: code, TotalQty: 200, AvailableQty: 200, CostPrice: model.FromFloat(5.07)}},
		Actor: "mcp-agent"}); err != nil {
		t.Fatalf("组合同步失败: %v", err)
	}
	p := mustPos(t, s, ctx, code)
	if p.FirstOpenDate != "20261012" {
		t.Errorf("组合同步把建仓日改成了 %q, 期望仍为 20261012（同步绝不清空建仓日）", p.FirstOpenDate)
	}
	if p.TotalQty != 200 || p.CostPrice != model.FromFloat(5.07) {
		t.Errorf("校准必须照常生效: total=%d cost=%s", int64(p.TotalQty), p.CostPrice)
	}

	// 再同步一次也不能改变它（每天跑都不复发）
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(0), Cash: model.FromFloat(10163),
		Items: []PortfolioInput{{TsCode: code, TotalQty: 200, AvailableQty: 200, CostPrice: model.FromFloat(4.85)}},
		Actor: "mcp-agent"}); err != nil {
		t.Fatalf("二次组合同步失败: %v", err)
	}
	if got := mustPos(t, s, ctx, code).FirstOpenDate; got != "20261012" {
		t.Errorf("二次同步后建仓日=%q, 期望仍为 20261012", got)
	}
}

// TestSyncPortfolioOpenDateRules 建仓日的四种情形各一条断言（规则见 syncPositionOpenDate）。
func TestSyncPortfolioOpenDateRules(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()

	// 一、建仓日已丢失（本次事故留下的脏数据）→ 按同步日补齐，保护立刻回来
	lost := "sh601766"
	tkLost := seedTicketOn(t, s, lost, model.DirBuy, 100, model.FromFloat(6.39), "20261015")
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tkLost.ID, Qty: 100, Price: model.FromFloat(6.39)}); err != nil {
		t.Fatalf("建仓回执失败: %v", err)
	}
	if _, err := s.WriteDB().Exec(`UPDATE position SET first_open_date='' WHERE ts_code=?`, lost); err != nil {
		t.Fatalf("制造丢失建仓日的脏数据失败: %v", err)
	}
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(0), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: lost, TotalQty: 100, AvailableQty: 100, CostPrice: model.FromFloat(6.44)}},
		Actor: "test"}); err != nil {
		t.Fatalf("组合同步失败: %v", err)
	}
	if got := mustPos(t, s, ctx, lost).FirstOpenDate; got != "20261016" {
		t.Errorf("丢失的建仓日未被补齐: %q, 期望 20261016（按同步日起算，宁可低估持有期）", got)
	}

	// 二、显式给出建仓日＝人工纠错通道，优先级最高
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(0), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: lost, TotalQty: 100, AvailableQty: 100,
			CostPrice: model.FromFloat(6.44), FirstOpenDate: "20261015"}},
		Actor: "test"}); err != nil {
		t.Fatalf("纠错同步失败: %v", err)
	}
	if got := mustPos(t, s, ctx, lost).FirstOpenDate; got != "20261015" {
		t.Errorf("显式建仓日没生效: %q, 期望 20261015", got)
	}

	// 三、清仓行（total_qty=0）留着上一轮的建仓日：重开仓必须起算新日期，不沿用旧日期
	reopened := "sh600908"
	if err := s.TradeRepo().UpsertPosition(ctx, model.Position{
		TsCode: reopened, TotalQty: 0, CostPrice: model.FromFloat(6), HighPrice: model.FromFloat(6),
		FirstOpenDate: "20261008",
	}); err != nil {
		t.Fatalf("播种清仓行失败: %v", err)
	}
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(0), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: reopened, TotalQty: 100, AvailableQty: 0, CostPrice: model.FromFloat(6.1)}},
		Actor: "test"}); err != nil {
		t.Fatalf("重开仓同步失败: %v", err)
	}
	if got := mustPos(t, s, ctx, reopened).FirstOpenDate; got != "20261016" {
		t.Errorf("清仓后重开的建仓日=%q, 期望 20261016（上一轮的 20261008 属于已结束的持仓）", got)
	}

	// 四、券商同步回来一只不持仓的票：建仓日原样保留，既不清空也不重新盖章
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261019",
		Capital: model.FromFloat(0), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: reopened, TotalQty: 0, CostPrice: model.FromFloat(6.1)}},
		Actor: "test"}); err != nil {
		t.Fatalf("零持仓同步失败: %v", err)
	}
	if got := mustPos(t, s, ctx, reopened).FirstOpenDate; got != "20261016" {
		t.Errorf("不持仓同步改动了建仓日: %q, 期望仍为 20261016", got)
	}
}

// TestSyncPortfolioRejectsMalformedOpenDate 建仓日格式非法必须整次同步拒绝，且账本零改动。
//
// 为什么这条值得单独守：持有天数按字符串比较算出，"2026-10-12" 比任何 YYYYMMDD 都小，
// 会被算成"已持有几百天"，最小持有期保护反向失效——错格式的日期比没有日期更坏。
func TestSyncPortfolioRejectsMalformedOpenDate(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	code := "sh600020"
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(15000),
		Items: []PortfolioInput{{TsCode: code, TotalQty: 100, AvailableQty: 100, CostPrice: model.FromFloat(10)}},
		Actor: "test"}); err != nil {
		t.Fatalf("播种同步失败: %v", err)
	}
	before := mustPos(t, s, ctx, code)

	for _, bad := range []string{"2026-10-12", "2026101", "202610120", "20261X12", "10-12-2026"} {
		if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261017",
			Capital: model.FromFloat(30000), Cash: model.FromFloat(18000),
			Items: []PortfolioInput{{TsCode: code, TotalQty: 999, AvailableQty: 999,
				CostPrice: model.FromFloat(11), FirstOpenDate: bad}},
			Actor: "test"}); err == nil {
			t.Errorf("非法建仓日 %q 应被拒绝", bad)
		}
	}
	// 拒绝必须发生在动本金/现金锚点/持仓之前：三处都还是第一次同步的值
	if after := mustPos(t, s, ctx, code); after != before {
		t.Errorf("被拒绝的同步改动了持仓行: before=%+v after=%+v", before, after)
	}
	all, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if all["account.initial_capital"] != "20000" {
		t.Errorf("本金被被拒绝的同步改成了 %s, 期望 20000", all["account.initial_capital"])
	}
	if all["account.cash_anchor"] != "1500000" {
		t.Errorf("现金锚点被被拒绝的同步改成了 %s, 期望 1500000（分）", all["account.cash_anchor"])
	}
}

// TestReportFillNeverLosesOpenDate 回执链路对建仓日同样只能保住或显式新开：
// 部分卖出、全部卖出、清仓后重开，三条路径各断言一次。
func TestReportFillNeverLosesOpenDate(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	code := "sh600021"

	buy := seedTicketOn(t, s, code, model.DirBuy, 500, model.FromFloat(10), "20261012")
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: buy.ID, Qty: 500, Price: model.FromFloat(10)}); err != nil {
		t.Fatal(err)
	}

	// 部分卖出：建仓日不动
	sell1 := seedTicketOn(t, s, code, model.DirSell, 200, model.FromFloat(11), "20261013")
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: sell1.ID, Qty: 200, Price: model.FromFloat(11)}); err != nil {
		t.Fatal(err)
	}
	p := mustPos(t, s, ctx, code)
	if p.FirstOpenDate != "20261012" || p.TotalQty != 300 {
		t.Fatalf("部分卖出后 total=%d 建仓日=%q, 期望 300/20261012", int64(p.TotalQty), p.FirstOpenDate)
	}

	// 全部卖出：行留在库里（0 股），建仓日不碍事
	sell2 := seedTicketOn(t, s, code, model.DirSell, 300, model.FromFloat(11), "20261014")
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: sell2.ID, Qty: 300, Price: model.FromFloat(11)}); err != nil {
		t.Fatal(err)
	}
	if p = mustPos(t, s, ctx, code); p.TotalQty != 0 {
		t.Fatalf("清仓后 total=%d, 期望 0", int64(p.TotalQty))
	}

	// 清仓后重开：建仓日必须是新的一天，不能沿用上一轮
	buy2 := seedTicketOn(t, s, code, model.DirBuy, 400, model.FromFloat(12), "20261020")
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: buy2.ID, Qty: 400, Price: model.FromFloat(12)}); err != nil {
		t.Fatal(err)
	}
	if got := mustPos(t, s, ctx, code).FirstOpenDate; got != "20261020" {
		t.Errorf("重开仓后建仓日=%q, 期望 20261020", got)
	}
}

// TestSyncPortfolioRejectsUnitErrorPrice 复现 20261015 的量纲事故（agent_issues #12-A）：
// 券商口径 5.07 元的成本被传成 507。成本一旦被写进 position，止损线就变成 507×0.92≈466 元，
// 之后每轮扫描都判"已跌破止损"，直接误发 M3 紧急邮件惊扰用户。
// 防呆必须落在动账本之前：持仓行、本金、现金锚点一处都不许留下痕迹。
func TestSyncPortfolioRejectsUnitErrorPrice(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	code := "sh600704"
	seedBar(t, s, code, "20261015", model.FromFloat(4.85)) // 参照：最近未复权收盘

	// 100 倍量纲错：507 元本身是"合法价格"，只有跟参考价一比才露馅
	_, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261015",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(10163),
		Items: []PortfolioInput{{TsCode: code, TotalQty: 200, AvailableQty: 200, CostPrice: model.FromFloat(507)}},
		Actor: "test"})
	if !errors.Is(err, ErrPriceSanity) {
		t.Fatalf("成本放大 100 倍应整次拒绝并返回 ErrPriceSanity, 实际: %v", err)
	}
	// 同一只票的正常口径必须过得去（防呆不能挡真实数据）
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261015",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(10163),
		Items: []PortfolioInput{{TsCode: code, TotalQty: 200, AvailableQty: 200, CostPrice: model.FromFloat(5.07)}},
		Actor: "test"}); err != nil {
		t.Fatalf("5.07 元（正确量纲）被误拒: %v", err)
	}
	if got := mustPos(t, s, ctx, code).CostPrice; got != model.FromFloat(5.07) {
		t.Errorf("成本=%s, 期望 5.07", got)
	}
}

// TestSyncPortfolioRejectsOutOfRangePrices 绝对区间这道闸：没有参考价时它独自生效。
func TestSyncPortfolioRejectsOutOfRangePrices(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()

	cases := []struct {
		name  string
		items []PortfolioInput
	}{
		// 无日线（参考价缺失）也要挡住的万元级成本
		{"成本万元级", []PortfolioInput{{TsCode: "sh600030", TotalQty: 100, CostPrice: model.FromFloat(20000)}}},
		// 高点被写成 5.07 元对应的"分"值 507 → 与成本相差百倍：库里没这只票的日线，
		// 507 元落在绝对区间内，但 high_price 与 cost_price 的比在这里不互查，
		// 所以这条用的是真正越界的 50700 元
		{"高点越界", []PortfolioInput{{TsCode: "sh600031", TotalQty: 100, CostPrice: model.FromFloat(5.07),
			HighPrice: model.FromFloat(50700)}}},
	}
	for _, c := range cases {
		_, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261015",
			Capital: model.FromFloat(20000), Cash: model.FromFloat(10163), Items: c.items, Actor: "test"})
		if !errors.Is(err, ErrPriceSanity) {
			t.Errorf("%s: 应返回 ErrPriceSanity, 实际: %v", c.name, err)
		}
	}
	// 全都被拒 → 账本一处没写：持仓空、本金与现金锚点都没落
	ps, err := s.TradeRepo().ListPositions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("被拒绝的同步写出了 %d 行持仓", len(ps))
	}
	raw, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if raw["account.initial_capital"] != "" || raw["account.cash_anchor"] != "" {
		t.Errorf("被拒绝的同步动了资金口径: capital=%q anchor=%q",
			raw["account.initial_capital"], raw["account.cash_anchor"])
	}
}

// TestReportFillRejectsUnitErrorPrice 成交回执同样把量纲错挡在记账之前：
// 单据参考价 5.05 元，回报 505 元＝100 倍错，指令单必须留在 issued、持仓不产生。
func TestReportFillRejectsUnitErrorPrice(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()
	tk := seedTicket(t, s, "sh600032", model.DirBuy, 200, model.FromFloat(5.05))

	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tk.ID, Qty: 200, Price: model.FromFloat(505)}); !errors.Is(err, ErrPriceSanity) {
		t.Fatalf("回报价放大 100 倍应返回 ErrPriceSanity, 实际: %v", err)
	}
	after, err := s.TradeRepo().GetTicket(ctx, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != model.TicketIssued || after.HasFill() {
		t.Errorf("被拒绝的回执动了指令单: status=%s fill=%v", after.Status, after.HasFill())
	}
	if _, err := s.TradeRepo().GetPosition(ctx, tk.TsCode); err == nil {
		t.Errorf("被拒绝的回执写出了持仓")
	}
	// 正确量纲照常成交
	if _, err := led.ReportFill(ctx, FillRequest{TicketID: tk.ID, Qty: 200, Price: model.FromFloat(5.07)}); err != nil {
		t.Fatalf("5.07 元（正确量纲）被误拒: %v", err)
	}
}

// TestSyncPortfolioRequiresCostForHoldings 有持仓必须给成本（组长 20261016 定 cost_price 必填）：
// 缺了会落 0 成本，止损线被算成 0×(1−8%)=0，永不下穿——价格型退出保护静默消失，
// 比量纲传错更隐蔽（单位错至少还有 M3 误报能暴露）。已卖光的行（total_qty=0）不给成本必须放行：
// 它既没持仓也没止损，硬要成本只会逼调用方往闭合行里编一个没人核对过的数字。
func TestSyncPortfolioRequiresCostForHoldings(t *testing.T) {
	s, led := openLedger(t)
	ctx := context.Background()

	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: "sh600040", TotalQty: 200, AvailableQty: 200}},
		Actor: "test"}); !errors.Is(err, ErrCostMissing) {
		t.Fatalf("持有 200 股却不给成本应返回 ErrCostMissing, 实际: %v", err)
	}
	// 成本给成负数同样是"没有可用成本"，一起挡在写库之前
	if _, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: "sh600040", TotalQty: 200, CostPrice: model.FromFloat(-1)}},
		Actor: "test"}); !errors.Is(err, ErrCostMissing) {
		t.Errorf("负成本应返回 ErrCostMissing, 实际: %v", err)
	}
	ps, err := s.TradeRepo().ListPositions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 0 {
		t.Errorf("被拒绝的同步写出了 %d 行持仓", len(ps))
	}
	raw, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if raw["account.initial_capital"] != "" {
		t.Errorf("被拒绝的同步动了本金: %s", raw["account.initial_capital"])
	}

	// 一、给了成本的持仓照常通过
	if n, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(20000), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: "sh600040", TotalQty: 200, AvailableQty: 200, CostPrice: model.FromFloat(5.07)}},
		Actor: "test"}); err != nil || n != 1 {
		t.Fatalf("合法成本被拒或计数错: n=%d err=%v", n, err)
	}
	// 二、券商报已卖光（total_qty=0）不给成本必须放行
	if n, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(0), Cash: model.FromFloat(9000),
		Items: []PortfolioInput{{TsCode: "sh600040", TotalQty: 0}},
		Actor: "test"}); err != nil || n != 1 {
		t.Errorf("已卖光的行不该被成本必填挡住: n=%d err=%v", n, err)
	}
	// 三、只校准资金口径（Items 为空）不受这条影响
	if n, _, err := led.SyncPortfolio(ctx, PortfolioSync{Date: "20261016",
		Capital: model.FromFloat(0), Cash: model.FromFloat(9500), Actor: "test"}); err != nil || n != 0 {
		t.Errorf("空 Items 只校准资金: n=%d err=%v", n, err)
	}
}

// mustPos 读持仓（测试辅助）。
func mustPos(t *testing.T, s *store.Store, ctx context.Context, code string) model.Position {
	t.Helper()
	p, err := s.TradeRepo().GetPosition(ctx, code)
	if err != nil {
		t.Fatalf("读持仓 %s 失败: %v", code, err)
	}
	return p
}
