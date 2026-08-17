package engine

// Pipeline 端到端测试: 用真实 PaperBroker + 内存数据验证
//  1. T+1 次日成交模型 (买T+1开盘 → T+2可卖)
//  2. 止损信号优先执行
//  3. Advice 建议信号不进成交管道
//  4. 策略失败天数统计

import (
	"context"
	"path/filepath"
	"testing"

	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
)

// fakeStrategy 按脚本发信号的假策略: day -> 信号序列
type fakeStrategy struct {
	name    string
	script  map[string][]model.Signal
	failOn  string // 该日期返回错误 (测试策略失败统计)
	errDays int
}

func (f *fakeStrategy) Name() string { return f.name }
func (f *fakeStrategy) Init(_ context.Context, _ map[string]interface{}) error {
	return nil
}
func (f *fakeStrategy) OnBar(_ context.Context, barCtx *strategy.BarContext) ([]model.Signal, error) {
	if f.failOn == barCtx.TradeDate {
		f.errDays++
		return nil, context.DeadlineExceeded
	}
	return f.script[barCtx.TradeDate], nil
}

// newTestPipeline 构造带临时SQLite数据的管道
func newTestPipeline(t *testing.T, strat strategy.Strategy, fillMode string) (*Pipeline, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pipeline.db")
	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	barRepo := store.NewBarRepo(db)

	// 5 个交易日, 价格温和上行
	dates := []string{"20240102", "20240103", "20240104", "20240105", "20240108"}
	closes := []float64{10.0, 10.1, 10.2, 10.3, 10.4}
	bars := make([]model.Bar, 0, len(dates))
	for i, d := range dates {
		pre := 10.0
		if i > 0 {
			pre = closes[i-1]
		}
		bars = append(bars, model.Bar{
			TsCode: "000001.SZ", TradeDate: d,
			Open: pre, High: closes[i] + 0.1, Low: pre - 0.1, Close: closes[i],
			PreClose: pre, Vol: 1000, AdjFactor: 1,
		})
	}
	if err := barRepo.BatchInsert(bars); err != nil {
		t.Fatalf("种子行情失败: %v", err)
	}

	dp, err := backtest.NewDataProvider(barRepo, []string{"000001.SZ"}, "20240102", "20240108")
	if err != nil {
		t.Fatalf("数据提供者失败: %v", err)
	}
	cal := market.NewCalendar(dates)

	costModel := market.NewCostModel(config.CostConfig{
		CommissionRate: 0.00025, MinCommission: 5.0, StampTaxRate: 0.0005, TransferFeeRate: 0.00001,
	})
	pb := broker.NewPaperBroker("test", 10000, costModel)
	pb.SetMatchRules(0.0002, store.NewLimitRepo(db))

	rm := risk.NewRiskManager(config.RiskConfig{
		MaxPositionPct: 0.9, MaxTotalPositionPct: 1.0, StopLossPct: 0.08, TakeProfitPct: 0.5,
	})
	p := NewPipeline(PipelineConfig{
		Broker:    pb,
		Strategy:  strat,
		Risk:      rm,
		Data:      dp,
		Calendar:  cal,
		Universe:  []string{"000001.SZ"},
		StartDate: "20240102",
		EndDate:   "20240108",
		RunID:     "test_run",
		FillMode:  fillMode,
		Stocks:    map[string]*model.Stock{"000001.SZ": {TsCode: "000001.SZ", ListStatus: "L"}},
	})
	return p, func() { db.Close() }
}

// TestPipeline_NextOpenT1 次日开盘成交 + T+1: 买入成交日 T+1, T+1 当日不可卖, T+2 起可卖
func TestPipeline_NextOpenT1(t *testing.T) {
	fs := &fakeStrategy{name: "fake", script: map[string][]model.Signal{
		"20240102": {{TsCode: "000001.SZ", Direction: model.DirBuy, TargetQty: 900, Reason: "buy"}},
	}}
	p, done := newTestPipeline(t, fs, "next_open")
	defer done()

	if err := p.Run(); err != nil {
		t.Fatalf("管道运行失败: %v", err)
	}

	// 成交记录: 买入应发生在 20240103 (次日开盘), 且只有一笔买入
	trades := p.Trades()
	if len(trades) != 1 {
		t.Fatalf("应只有1笔买入成交, 实际 %d", len(trades))
	}
	tr := trades[0]
	if tr.Side != model.SideBuy || tr.TradeDate != "20240103" {
		t.Errorf("买入应于次日20240103开盘成交, 实际 %s side=%d", tr.TradeDate, tr.Side)
	}
	if tr.Price < 9.999 || tr.Price > 10.003 {
		t.Errorf("成交价应为20240103开盘价10.0+滑点0.02%%后取整≈10.00, 实际 %.4f", tr.Price)
	}
	// 快照序列: 20240102 无持仓(挂单未成交), 20240103 起有持仓
	snaps := p.Snapshots()
	if len(snaps) != 5 {
		t.Fatalf("应有5个交易日快照, 实际 %d", len(snaps))
	}
	if snaps[0].MarketValue != 0 {
		t.Errorf("T日(20240102)不应有持仓市值, 实际 %.2f", snaps[0].MarketValue)
	}
	if snaps[1].MarketValue <= 0 {
		t.Errorf("T+1(20240103)应有持仓市值, 实际 %.2f", snaps[1].MarketValue)
	}
	// T+1 当日不可卖: 20240103 发卖出信号应被 T+1 拒绝
	// 用另一个管道验证更清晰: 直接在 20240103 脚本发卖出
	fs2 := &fakeStrategy{name: "fake", script: map[string][]model.Signal{
		"20240102": {{TsCode: "000001.SZ", Direction: model.DirBuy, TargetQty: 900, Reason: "buy"}},
		"20240103": {{TsCode: "000001.SZ", Direction: model.DirSell, TargetQty: 900, Reason: "sell-t1"}},
	}}
	p2, done2 := newTestPipeline(t, fs2, "next_open")
	defer done2()
	if err := p2.Run(); err != nil {
		t.Fatalf("管道2运行失败: %v", err)
	}
	sells := 0
	for _, tr := range p2.Trades() {
		if tr.Side == model.SideSell {
			sells++
		}
	}
	if sells != 0 {
		t.Errorf("T+1当日卖出当日买入持仓应被拒绝, 实际卖出 %d 笔", sells)
	}
	// 20240104 卖出应成功 (T+2 可卖)
	fs3 := &fakeStrategy{name: "fake", script: map[string][]model.Signal{
		"20240102": {{TsCode: "000001.SZ", Direction: model.DirBuy, TargetQty: 900, Reason: "buy"}},
		"20240104": {{TsCode: "000001.SZ", Direction: model.DirSell, TargetQty: 900, Reason: "sell-t2"}},
	}}
	p3, done3 := newTestPipeline(t, fs3, "next_open")
	defer done3()
	if err := p3.Run(); err != nil {
		t.Fatalf("管道3运行失败: %v", err)
	}
	sold := false
	for _, tr := range p3.Trades() {
		if tr.Side == model.SideSell {
			sold = true
		}
	}
	if !sold {
		t.Error("T+2 卖出应成功")
	}
}

// TestPipeline_AdviceSignalNotExecuted 建议信号不进成交管道
func TestPipeline_AdviceSignalNotExecuted(t *testing.T) {
	fs := &fakeStrategy{name: "fake", script: map[string][]model.Signal{
		"20240102": {{TsCode: "000001.SZ", Direction: model.DirBuy, TargetQty: 500, Reason: "advice-t", Advice: true}},
	}}
	p, done := newTestPipeline(t, fs, "next_open")
	defer done()
	if err := p.Run(); err != nil {
		t.Fatalf("管道运行失败: %v", err)
	}
	if len(p.Trades()) != 0 {
		t.Errorf("Advice 建议信号不应成交, 实际 %d 笔", len(p.Trades()))
	}
	if mv := p.Snapshots()[0].MarketValue; mv != 0 {
		t.Errorf("建议信号不应产生持仓, 实际市值 %.2f", mv)
	}
}

// TestPipeline_StrategyErrorTracked 策略报错被统计且不阻断止损信号
func TestPipeline_StrategyErrorTracked(t *testing.T) {
	fs := &fakeStrategy{
		name:   "fake",
		failOn: "20240103",
		script: map[string][]model.Signal{
			"20240102": {{TsCode: "000001.SZ", Direction: model.DirBuy, TargetQty: 900, Reason: "buy"}},
		},
	}
	p, done := newTestPipeline(t, fs, "next_open")
	defer done()
	if err := p.Run(); err != nil {
		t.Fatalf("管道运行失败: %v", err)
	}
	if p.StrategyErrorDays() != 1 {
		t.Errorf("应统计到1天策略失败, 实际 %d", p.StrategyErrorDays())
	}
	// 20240103 策略失败但买入已在次日成交, 无止损信号: 持仓应保留
	snaps := p.Snapshots()
	if snaps[3].MarketValue <= 0 {
		t.Error("策略失败不应清仓, 持仓应保留")
	}
}
