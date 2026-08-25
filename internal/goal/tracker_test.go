package goal

import (
	"math"
	"path/filepath"
	"testing"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

func newTestTracker(t *testing.T, snaps []model.AccountSnapshot) (*Tracker, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := store.NewDB(dbPath)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	tradeRepo := store.NewTradeRepo(db)
	for _, s := range snaps {
		if err := tradeRepo.InsertAccountSnapshot("live", s); err != nil {
			t.Fatalf("插入快照失败: %v", err)
		}
	}
	portRepo := store.NewPortfolioRepo(db)
	portRepo.SetMeta("initial_capital", "10000")
	cfg := config.GoalConfig{
		Enabled:            true,
		QuarterlyTargetPct: 0.15,
		MaxDrawdownBudget:  0.10,
		AutoAdjust:         true,
	}
	return NewTracker(cfg, tradeRepo, portRepo, "live"), func() { db.Close() }
}

func TestQuarterOf(t *testing.T) {
	label, start, end := QuarterOf("20260817")
	if label != "2026Q3" || start != "20260701" || end != "20260930" {
		t.Errorf("季度划分错误: %s %s %s", label, start, end)
	}
	label, start, end = QuarterOf("20260101")
	if label != "2026Q1" || start != "20260101" || end != "20260331" {
		t.Errorf("Q1划分错误: %s %s %s", label, start, end)
	}
}

func TestStatus_Normal(t *testing.T) {
	snaps := []model.AccountSnapshot{
		{TradeDate: "20260630", TotalAsset: 11000},
		{TradeDate: "20260814", TotalAsset: 11200},
	}
	tr, done := newTestTracker(t, snaps)
	defer done()
	st, err := tr.Status("20260817", 11300)
	if err != nil {
		t.Fatal(err)
	}
	if st.BaselineAsset != 11000 {
		t.Errorf("季初基准应为11000, 实际 %.0f", st.BaselineAsset)
	}
	wantRet := 300.0 / 11000.0
	if math.Abs(st.ReturnPct-wantRet) > 1e-9 {
		t.Errorf("收益率错误: %.4f want %.4f", st.ReturnPct, wantRet)
	}
	if st.Mode != ModeNormal {
		t.Errorf("应为正常模式, 实际 %s", st.Mode)
	}
	if st.PeakAsset != 11300 {
		t.Errorf("峰值应含当前资产, 实际 %.0f", st.PeakAsset)
	}
}

func TestStatus_DefensiveOnBudgetExhausted(t *testing.T) {
	snaps := []model.AccountSnapshot{
		{TradeDate: "20260630", TotalAsset: 10000},
		{TradeDate: "20260801", TotalAsset: 11000}, // 峰值
		{TradeDate: "20260814", TotalAsset: 9900},  // 回撤10% = 预算耗尽
	}
	tr, done := newTestTracker(t, snaps)
	defer done()
	st, err := tr.Status("20260817", 9900)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(st.DrawdownPct-0.10) > 1e-9 {
		t.Errorf("回撤应为10%%, 实际 %.4f", st.DrawdownPct)
	}
	if st.Mode != ModeDefensive {
		t.Errorf("应为防守模式, 实际 %s", st.Mode)
	}
	// 验证风控收紧
	base := config.RiskConfig{MaxTotalPositionPct: 0.9, StopLossPct: 0.08}
	adj, notes := tr.AdjustRisk(base, st)
	if adj.MaxTotalPositionPct != 0.2 || adj.StopLossPct != 0.05 {
		t.Errorf("防守模式收紧不符预期: %+v", adj)
	}
	if len(notes) == 0 {
		t.Error("应有调整说明")
	}
}

func TestStatus_Tightened(t *testing.T) {
	snaps := []model.AccountSnapshot{
		{TradeDate: "20260630", TotalAsset: 10000},
		{TradeDate: "20260801", TotalAsset: 11000},
		{TradeDate: "20260814", TotalAsset: 10200}, // 回撤 ~7.3% → 消耗 72.7%
	}
	tr, done := newTestTracker(t, snaps)
	defer done()
	st, _ := tr.Status("20260817", 10200)
	if st.Mode != ModeTightened {
		t.Errorf("应为收紧模式, 实际 %s (回撤 %.2f%%)", st.Mode, st.DrawdownPct*100)
	}
	base := config.RiskConfig{MaxTotalPositionPct: 0.9}
	adj, _ := tr.AdjustRisk(base, st)
	if math.Abs(adj.MaxTotalPositionPct-0.54) > 1e-9 {
		t.Errorf("收紧后总仓位应为0.54, 实际 %.2f", adj.MaxTotalPositionPct)
	}
}

func TestStatus_LockedOnTargetAchieved(t *testing.T) {
	snaps := []model.AccountSnapshot{
		{TradeDate: "20260630", TotalAsset: 10000},
		{TradeDate: "20260814", TotalAsset: 11600}, // +16% > 15% 目标
	}
	tr, done := newTestTracker(t, snaps)
	defer done()
	st, _ := tr.Status("20260817", 11600)
	if st.Mode != ModeLocked {
		t.Errorf("应为锁利模式, 实际 %s", st.Mode)
	}
	if st.Progress < 1.0 {
		t.Errorf("进度应>=1, 实际 %.2f", st.Progress)
	}
}

func TestStatus_NoSnapshots(t *testing.T) {
	tr, done := newTestTracker(t, nil)
	defer done()
	st, err := tr.Status("20260817", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if st.BaselineAsset != 10000 {
		t.Errorf("无快照时应退回初始资金10000, 实际 %.0f", st.BaselineAsset)
	}
	if st.Mode != ModeNormal {
		t.Errorf("应为正常模式, 实际 %s", st.Mode)
	}
}

func TestAdjustRisk_NeverLoosens(t *testing.T) {
	tr, done := newTestTracker(t, nil)
	defer done()
	// 基础配置已经很严: 不应被放松
	base := config.RiskConfig{MaxTotalPositionPct: 0.1, StopLossPct: 0.03}
	st := &Status{Mode: ModeDefensive}
	adj, _ := tr.AdjustRisk(base, st)
	if adj.MaxTotalPositionPct != 0.1 || adj.StopLossPct != 0.03 {
		t.Errorf("只收紧不放松原则被破坏: %+v", adj)
	}
	// 未启用 auto_adjust 时原样返回
	tr2 := NewTracker(config.GoalConfig{Enabled: true, AutoAdjust: false}, nil, nil, "live")
	adj2, notes := tr2.AdjustRisk(base, st)
	if adj2 != base || len(notes) != 0 {
		t.Error("auto_adjust=false 时不应调整")
	}
}
