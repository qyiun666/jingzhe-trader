package signal

import (
	"context"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// sellFixture 建一个够 sellDecisions 跑完的测试库：
// 一只持仓 600000.SH（成本/高点 10 元，建仓 20260901）、足够的交易日历、
// 20261021 的收盘价与 stock_basic 名称。20261021 距建仓已 11 个交易日，
// 满足 rank_out_min_hold_days=10。
func sellFixture(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/sell.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	for _, d := range []string{
		"20260901", "20260902", "20260903", "20260904",
		"20260907", "20260908", "20260909", "20260910", "20260911",
		"20260914", "20260915", "20261021",
	} {
		if err := st.MarketRepo().UpsertCal(ctx, store.CalRow{CalDate: d, IsOpen: true}); err != nil {
			t.Fatalf("写交易日历 %s 失败: %v", d, err)
		}
	}
	if err := st.MarketRepo().UpsertStockBasic(ctx, model.StockBasic{
		TsCode: "600000.SH", Name: "测试股", ListStatus: "L",
	}); err != nil {
		t.Fatalf("写 stock_basic 失败: %v", err)
	}
	if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
		TsCode: "600000.SH", TradeDate: "20261021",
		Close: 1000, VolLot: 10000, RawClose: 1000,
	}); err != nil {
		t.Fatalf("写日线失败: %v", err)
	}
	if err := st.TradeRepo().UpsertPosition(ctx, model.Position{
		TsCode: "600000.SH", TotalQty: 100,
		CostPrice: 1000, HighPrice: 1000, FirstOpenDate: "20260901",
	}); err != nil {
		t.Fatalf("写持仓失败: %v", err)
	}
	return st
}

// runSellDecisions 以固定持仓跑一次 sellDecisions（20261021）。
func runSellDecisions(t *testing.T, st *store.Store, gate store.MarketGate,
	cands, watch []model.Candidate, idx indexInfo) ([]model.Signal, *Report) {
	t.Helper()
	s := NewService(st, &ticket.Ledger{}, gate, nil, RankOutPolicy{MinHoldDays: 10})
	positions, err := st.TradeRepo().ListPositions(context.Background())
	if err != nil {
		t.Fatalf("读持仓失败: %v", err)
	}
	rep := &Report{}
	sells, err := s.sellDecisions(context.Background(), "20261021", cands, watch, positions,
		risk.DefaultParams(model.FromFloat(100000)), idx, rep)
	if err != nil {
		t.Fatalf("sellDecisions 失败: %v", err)
	}
	return sells, rep
}

// TestSellDecisionsEmptyBandSkipsRankOut 非关闸日 0 候选：无榜不是跌出榜，
// 满持有期的持仓也不得触发排名淘汰；必须落 RankBandEmpty 供上层降级。
func TestSellDecisionsEmptyBandSkipsRankOut(t *testing.T) {
	st := sellFixture(t)
	sells, rep := runSellDecisions(t, st, store.MarketGate{Enabled: false},
		nil, nil, indexInfo{})

	if len(sells) != 0 {
		t.Fatalf("空 band 不应产出卖单，实际 %+v", sells)
	}
	if !rep.RankBandEmpty {
		t.Error("空 band 必须置 RankBandEmpty=true")
	}
	notes := strings.Join(rep.Notes, "\n")
	if !strings.Contains(notes, "RANK_BAND_EMPTY") {
		t.Errorf("缺少 RANK_BAND_EMPTY 说明，notes=%v", rep.Notes)
	}
	// 空 band 时规则 4 整体不适用，不应再写"未进保留带但被最小持有期豁免"
	// 这类说明——那会让人误以为规则 4 跑过、只是被保护了。
	if strings.Contains(notes, "本次排名淘汰豁免") {
		t.Errorf("空 band 不应输出持有期豁免说明，notes=%v", rep.Notes)
	}
}

// TestSellDecisionsEmptyBandStillMarketBad 关闸日（gate=true、指数在均线下方）
// 没有 band：规则 4 跳过，但规则 5 必须照旧退出。
func TestSellDecisionsEmptyBandStillMarketBad(t *testing.T) {
	st := sellFixture(t)
	sells, rep := runSellDecisions(t, st, store.MarketGate{Enabled: true, Window: 60},
		nil, nil, indexInfo{close: model.FromFloat(3000), ma60: model.FromFloat(3100)})

	if len(sells) != 1 || sells[0].Rule != RuleMarketBad {
		t.Fatalf("关闸日空 band 应由规则 5 退出，实际 %+v", sells)
	}
	if !rep.RankBandEmpty {
		t.Error("空 band 必须置 RankBandEmpty=true")
	}
}

// TestSellDecisionsETFBandIsNotStockBand 当日只有 ETF 候选时，ETF 不能充当
// 个股的排名带；否则一只持仓会被误判为"跌出榜"。
func TestSellDecisionsETFBandIsNotStockBand(t *testing.T) {
	st := sellFixture(t)
	cands := []model.Candidate{{TsCode: "588000.SH", Kind: model.KindETF}}
	sells, rep := runSellDecisions(t, st, store.MarketGate{Enabled: false},
		cands, nil, indexInfo{})

	if len(sells) != 0 {
		t.Fatalf("ETF-only band 不应触发个股排名淘汰，实际 %+v", sells)
	}
	if !rep.RankBandEmpty {
		t.Error("ETF-only band 应视为无个股 band")
	}
}

// TestSellDecisionsNonEmptyBandStillRankOut 有正常个股 band 时回归旧口径：
// 持仓不在 band 内且已满最小持有期 → 仍排名淘汰。
func TestSellDecisionsNonEmptyBandStillRankOut(t *testing.T) {
	st := sellFixture(t)
	cands := []model.Candidate{{TsCode: "000001.SZ"}}
	sells, rep := runSellDecisions(t, st, store.MarketGate{Enabled: false},
		cands, nil, indexInfo{})

	if len(sells) != 1 || sells[0].Rule != RuleRankOut {
		t.Fatalf("有 band 时跌出榜应照旧排名淘汰，实际 %+v", sells)
	}
	if rep.RankBandEmpty {
		t.Error("有正常个股 band 时不该置 RankBandEmpty")
	}
}

// TestSeriesCodes 候选与保留带的代码并集：去重、去空，顺序保持先候选后保留带。
func TestSeriesCodes(t *testing.T) {
	cands := []model.Candidate{{TsCode: "A"}, {TsCode: "B"}, {TsCode: ""}}
	watch := []model.Candidate{{TsCode: "B"}, {TsCode: "C"}, {TsCode: "C"}}
	got := seriesCodes(cands, watch)
	if strings.Join(got, ",") != "A,B,C" {
		t.Errorf("代码并集=%v，期望 [A B C]", got)
	}
}

// TestBarSeriesReadsOnlyRequestedCodes 信号层不再按日期读全市场：
// 只回给定代码的日线，未请求的票即使同一天有日线也不得出现在结果里。
// 这是 P2-2 的回归锁——读法退回 BarCloseSeries 时，这条会因多出 600999.SH 而红。
func TestBarSeriesReadsOnlyRequestedCodes(t *testing.T) {
	st := sellFixture(t)
	ctx := context.Background()
	if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
		TsCode: "600999.SH", TradeDate: "20261021",
		Close: 2000, VolLot: 1, RawClose: 2000,
	}); err != nil {
		t.Fatalf("写未请求标的日线失败: %v", err)
	}
	s := NewService(st, &ticket.Ledger{}, store.MarketGate{}, nil, RankOutPolicy{})
	got, err := s.barSeries(ctx, "20261021", []string{"600000.SH"})
	if err != nil {
		t.Fatalf("barSeries 失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("只请求 1 只，实际返回 %d 只: %v", len(got), got)
	}
	if len(got["600000.SH"].Closes) == 0 {
		t.Error("请求标的的日线不应为空")
	}
	if _, ok := got["600999.SH"]; ok {
		t.Error("未请求标的混进了窗口序列（读法退回了全市场扫描）")
	}
}

// TestBudgetOfCarriesTradeCost 决策预算必须带上成本口径：模型在 prompt 里看到的
// 双边成本、最低净收益与单笔下限，必须与风控实际执行的是同一组数。
func TestBudgetOfCarriesTradeCost(t *testing.T) {
	p := risk.WeakParams(risk.DefaultParams(model.FromFloat(10000)), 0.20, 0.10)
	state := risk.AccountState{
		TotalAsset: model.FromFloat(10000), Cash: model.FromFloat(10000),
	}
	b := budgetOf(state, p, "600000.SH", model.FromFloat(10))
	if b.SlotFen != p.SingleCapFen() {
		t.Errorf("SlotFen=%s，期望单票上限 %s", b.SlotFen, p.SingleCapFen())
	}
	if b.MinAmountFen != p.MinAmountFloor() {
		t.Errorf("MinAmountFen=%s，期望风控生效下限 %s", b.MinAmountFen, p.MinAmountFloor())
	}
	if want := risk.RoundTripCostFen(b.SlotFen); b.RoundTripCostFen != want {
		t.Errorf("RoundTripCostFen=%s，期望按单票上限估算 %s", b.RoundTripCostFen, want)
	}
	if b.MinNetReturnFen != b.RoundTripCostFen*2 {
		t.Errorf("MinNetReturnFen=%s，期望 2×双边成本 %s", b.MinNetReturnFen, b.RoundTripCostFen*2)
	}
	if b.MinAmountFen <= b.SlotFen/2 {
		t.Errorf("弱势档单笔下限 %s 应高于单票上限的一半 %s", b.MinAmountFen, b.SlotFen/2)
	}
}
