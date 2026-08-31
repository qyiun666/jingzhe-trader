package api

import (
	"path/filepath"
	"testing"

	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// TestRestorePortfolioRefreshesMarketValue 重启恢复: 数量/成本以库为唯一源,
// 市值是派生值, 恢复后必须用库内最新收盘价现算, 不能带零
func TestRestorePortfolioRefreshesMarketValue(t *testing.T) {
	db, err := store.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	if err := store.NewBarRepo(db).BatchInsert([]model.Bar{
		{TsCode: "600519.SH", TradeDate: "20260827", Close: 1500},
		{TsCode: "000001.SZ", TradeDate: "20260820", Close: 10.5}, // 停牌, 之后再无行情
	}); err != nil {
		t.Fatalf("插入日线失败: %v", err)
	}

	portRepo := store.NewPortfolioRepo(db)
	if err := portRepo.SyncPortfolio([]store.PortfolioSyncItem{
		{TsCode: "600519.SH", TotalQty: 100, AvailableQty: 100, CostPrice: 1400},
		{TsCode: "000001.SZ", TotalQty: 1000, AvailableQty: 1000, CostPrice: 11},
	}); err != nil {
		t.Fatalf("写入持仓失败: %v", err)
	}
	if err := portRepo.SetMeta("cash", "5000"); err != nil {
		t.Fatalf("写入现金失败: %v", err)
	}

	s := &Service{
		cfg:     &config.Config{Backtest: config.BacktestConfig{InitialCapital: 100000}},
		db:      db,
		barRepo: store.NewBarRepo(db),
		brk:     broker.NewPaperBroker("paper", 100000, nil),
	}
	s.restorePortfolioFromDB()

	pb := s.brk.(*broker.PaperBroker)
	pos := pb.GetPositions()
	p1 := pos["600519.SH"]
	if p1 == nil || p1.MarketPrice != 1500 || p1.MarketValue != 150000 {
		t.Errorf("600519.SH 应按最新收盘 1500 估值, 实际 %+v", p1)
	}
	p2 := pos["000001.SZ"]
	if p2 == nil || p2.MarketPrice != 10.5 || p2.MarketValue != 10500 {
		t.Errorf("000001.SZ 应按停牌前收盘 10.5 估值, 实际 %+v", p2)
	}
	asset, err := s.brk.QueryAsset()
	if err != nil {
		t.Fatalf("查询资产失败: %v", err)
	}
	if want := 5000.0 + 150000 + 10500; asset.TotalAsset != want {
		t.Errorf("总资产应为 %.2f (现金+现算市值), 实际 %.2f", want, asset.TotalAsset)
	}
}

// TestBuildPortfolioValuesFromLatestBar 持仓估值必须用"各标的自身最新一根日线"
// 曾按 time.Now() 当天取行情: 收盘数据到位之前 (盘前总结、夜间任何读取) 当天是空的,
// 于是每个持仓退化成成本价, 浮盈恒为 0 —— 用户据此看不到已经在亏的仓位
func TestBuildPortfolioValuesFromLatestBar(t *testing.T) {
	db, err := store.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	if err := store.NewBarRepo(db).BatchInsert([]model.Bar{
		{TsCode: "510050.SH", TradeDate: "20260828", Close: 3.05},
		{TsCode: "510050.SH", TradeDate: "20260831", Close: 3.041},
	}); err != nil {
		t.Fatalf("插入日线失败: %v", err)
	}
	if err := store.NewPortfolioRepo(db).SyncPortfolio([]store.PortfolioSyncItem{
		{TsCode: "510050.SH", TotalQty: 500, AvailableQty: 500, CostPrice: 3.059},
	}); err != nil {
		t.Fatalf("写入持仓失败: %v", err)
	}

	s := &Service{cfg: &config.Config{}, db: db, barRepo: store.NewBarRepo(db)}
	got := s.BuildPortfolio()
	if len(got) != 1 {
		t.Fatalf("应返回1条持仓, 实际 %d", len(got))
	}
	p := got[0]
	if p.MarketPrice != 3.041 {
		t.Errorf("应按最新收盘 3.041 估值, 实际 %.3f", p.MarketPrice)
	}
	if diff := p.FloatingPnL + 9.0; diff > 0.01 || diff < -0.01 {
		t.Errorf("浮亏应约 -9 元, 实际 %.2f", p.FloatingPnL)
	}
	if p.FloatingPnLPct >= 0 {
		t.Errorf("浮盈比例应为负, 实际 %.6f", p.FloatingPnLPct)
	}
}

// TestBuildUpdateOptions 可选数据同步开关映射: sync_optional=false 时全关, true 时全开
func TestBuildUpdateOptions(t *testing.T) {
	db, err := store.NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	// 关闭: 不应携带任何可选同步
	svc := &Service{cfg: &config.Config{}, barRepo: store.NewBarRepo(db)}
	opts := svc.buildUpdateOptions()
	if opts.SyncNews || opts.SyncMoneyFlow || opts.SyncTopList || opts.SyncFina {
		t.Fatalf("sync_optional=false 时可选同步应全关: %+v", opts)
	}

	// 开启: 可选同步全开 (新闻/资金流/龙虎榜是辩论与选股的输入)
	svc.cfg.Dataloader.SyncOptional = true
	opts = svc.buildUpdateOptions()
	if !opts.SyncNews || !opts.SyncMoneyFlow || !opts.SyncTopList || !opts.SyncFina {
		t.Fatalf("sync_optional=true 时可选同步应全开: %+v", opts)
	}
}
