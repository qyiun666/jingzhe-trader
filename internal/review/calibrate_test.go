package review

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// openStoreForTestReview 打开一个临时库（store.Open 自动建全部现役表）。
func openStoreForTestReview(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	return st
}

// TestForwardReturnSkipsUnmatured 未到期的观察期必须标为"不可算"，不能用最近价凑。
func TestForwardReturnSkipsUnmatured(t *testing.T) {
	days := []string{"20260901", "20260902", "20260903", "20260904"}
	idx := map[string]int{}
	for i, d := range days {
		idx[d] = i
	}
	ser := map[string]float64{"20260901": 100, "20260902": 110, "20260904": 90}

	// 决策日 20260901 起第 5 个交易日 = 20260908 尚未到期（asOf=20260904）。
	if _, ok := forwardReturn(ser, days, idx, "20260901", 5, 100, "20260904", model.Calibration{}); ok {
		t.Error("尚未到期的观察期不该给出收益")
	}
	// 已落库的既成收益优先沿用，不重算。
	prev := model.Calibration{Ret5: 0.07, Has5: true}
	v, ok := forwardReturn(ser, days, idx, "20260901", 5, 100, "20260904", prev)
	if !ok || v != 0.07 {
		t.Errorf("应沿用已落库收益 0.07，得到 (%v,%v)", v, ok)
	}
}

// TestSummarizeLayered 分层统计：高置信组胜率高时 HasSignal 为真；样本不足时为假。
func TestSummarizeLayered(t *testing.T) {
	rows := make([]model.Calibration, 0, 20)
	// 高置信 buy：10 条，8 条正收益（胜率 80%）。
	for i := 0; i < 10; i++ {
		r := 0.05
		if i >= 8 {
			r = -0.03
		}
		rows = append(rows, model.Calibration{Verdict: "buy", Confidence: 0.8, Ret20: r, Has20: true})
	}
	// 低置信 buy：10 条，3 条正收益（胜率 30%）。
	for i := 0; i < 10; i++ {
		r := -0.04
		if i < 3 {
			r = 0.03
		}
		rows = append(rows, model.Calibration{Verdict: "buy", Confidence: 0.5, Ret20: r, Has20: true})
	}
	st := Summarize(rows)
	if st.Total != 20 || st.Buys != 20 {
		t.Fatalf("样本数 = %d/%d，期望 20/20", st.Total, st.Buys)
	}
	if st.HighConf.N != 10 || st.LowConf.N != 10 {
		t.Fatalf("分层 = %d/%d，期望 10/10", st.HighConf.N, st.LowConf.N)
	}
	if st.HighConf.WinRate < 0.79 || st.HighConf.WinRate > 0.81 {
		t.Errorf("高置信胜率 = %.2f，期望 0.80", st.HighConf.WinRate)
	}
	if st.LowConf.WinRate < 0.29 || st.LowConf.WinRate > 0.31 {
		t.Errorf("低置信胜率 = %.2f，期望 0.30", st.LowConf.WinRate)
	}
	if !st.HasSignal() {
		t.Error("胜率差 50pct 应判为有信号")
	}
}

// TestSummarizeNoSignalOnSmallSample 样本不足时绝不报"有信号"（防噪声误判）。
func TestSummarizeNoSignalOnSmallSample(t *testing.T) {
	rows := []model.Calibration{
		{Verdict: "buy", Confidence: 0.9, Ret20: 0.5, Has20: true},
		{Verdict: "buy", Confidence: 0.4, Ret20: -0.5, Has20: true},
	}
	st := Summarize(rows)
	if st.HasSignal() {
		t.Error("两组各 1 条票时任何差异都是噪声，不该判有信号")
	}
}

// TestSummarizeCountsMissedAndSkipsUnmatured 未到期行不参与统计；skip 上涨计入错过。
func TestSummarizeCountsMissedAndSkipsUnmatured(t *testing.T) {
	rows := []model.Calibration{
		{Verdict: "buy", Confidence: 0.8, Ret20: 0.1, Has20: true},
		{Verdict: "buy", Confidence: 0.8, Ret20: 0.9, Has20: false}, // 未到期
		{Verdict: "skip", Confidence: 0.5, Ret20: 0.2, Has20: true}, // 判不买而后涨
	}
	st := Summarize(rows)
	if st.Total != 2 {
		t.Errorf("Total = %d，期望 2（未到期行不算）", st.Total)
	}
	if st.Missed != 1 {
		t.Errorf("Missed = %d，期望 1", st.Missed)
	}
	if st.Buys != 1 {
		t.Errorf("Buys = %d，期望 1", st.Buys)
	}
}

// TestCalibratorEndToEnd 用真实 SQLite（新库）跑一轮归因：决策留痕 → 补收益 → 落归因行。
func TestCalibratorEndToEnd(t *testing.T) {
	st := openStoreForTestReview(t)
	defer st.Close()
	ctx := context.Background()
	repo := st.MarketRepo()

	// 6 个交易日日历。
	days := []string{"20260901", "20260902", "20260903", "20260904", "20260907", "20260908"}
	for _, d := range days {
		if err := repo.UpsertCal(ctx, store.CalRow{CalDate: d, IsOpen: true}); err != nil {
			t.Fatalf("写日失败: %v", err)
		}
	}
	// 决策日 20260901 买入 100，20 个交易日后的目标日在样本里够不到 → 只有 5 日到期。
	// 这里只造 6 天，故 5 日观察期落空、验证"未到期不硬算"；另造一条更早的决策看已到期口径。
	for i, d := range days {
		price := int64(1000 + i*10) // 20260901=1000 ... 20260908=1050
		if err := repo.UpsertBar(ctx, model.Bar{
			TsCode: "600000.SH", TradeDate: d, Close: model.Fen(price), RawClose: model.Fen(price),
		}); err != nil {
			t.Fatalf("写日线失败: %v", err)
		}
	}
	// 一条决策留痕（decision 行）。
	if err := st.LLMRepo().SaveCall(ctx, model.LLMCall{
		TradeDate: "20260901", TsCode: "600000.SH", PromptKey: "decision",
		Verdict: "buy", Confidence: 0.8, WeightPct: 0.3,
		Status: model.TraceOK, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("写决策留痕失败: %v", err)
	}

	cal := NewCalibrator(st).WithClock(func() time.Time { return time.Now() })
	st2, err := cal.Run(ctx, "20260908", 40)
	if err != nil {
		t.Fatalf("归因失败: %v", err)
	}
	// 6 个交易日不够 20 日观察期，Total 应为 0，但归因行必须已落库（供后续到期后追加）。
	if st2.Total != 0 {
		t.Errorf("样本窗口不足 20 日，Total 应为 0，实际 %d", st2.Total)
	}
	rows, err := st.TraceRepo().ListCalibrations(ctx, "20260901", "20260908")
	if err != nil {
		t.Fatalf("读归因行失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应落 1 条归因行，实际 %d", len(rows))
	}
	if rows[0].Verdict != "buy" || rows[0].Confidence != 0.8 {
		t.Errorf("归因行丢了决策信息: %+v", rows[0])
	}
	if rows[0].Has20 {
		t.Error("20 日观察期未到期，不该标记为已算")
	}

	// 幂等重跑：行数不增。
	if _, err := cal.Run(ctx, "20260908", 40); err != nil {
		t.Fatalf("二次归因失败: %v", err)
	}
	rows2, _ := st.TraceRepo().ListCalibrations(ctx, "20260901", "20260908")
	if len(rows2) != 1 {
		t.Errorf("重跑后归因行数 = %d，期望仍为 1（幂等覆盖）", len(rows2))
	}
}
