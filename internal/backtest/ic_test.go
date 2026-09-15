package backtest

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestRankICPerfectCorrelation 完全单调的两列，秩相关必须是 +1；反向必须 −1。
// 这是 IC 计算器的正例：如果连这个都算不对，后面所有结论都不可信。
func TestRankICPerfectCorrelation(t *testing.T) {
	f := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	r := []float64{10, 20, 30, 40, 50, 60, 70, 80}
	ic, ok := RankIC(f, r)
	if !ok {
		t.Fatal("完全单调序列应可计算")
	}
	if math.Abs(ic-1) > 1e-9 {
		t.Errorf("完全正相关 IC = %.6f，期望 1", ic)
	}
	rev := []float64{80, 70, 60, 50, 40, 30, 20, 10}
	ic, ok = RankIC(f, rev)
	if !ok || math.Abs(ic+1) > 1e-9 {
		t.Errorf("完全反相关 IC = %.6f (ok=%v)，期望 −1", ic, ok)
	}
}

// TestRankICRejectsDegenerate 负例：全并列表（无区分度）必须拒绝，而不是返回 0 或 NaN。
func TestRankICRejectsDegenerate(t *testing.T) {
	f := []float64{5, 5, 5, 5, 5}
	r := []float64{1, 2, 3, 4, 5}
	if _, ok := RankIC(f, r); ok {
		t.Error("因子列全并列时无区分度，应返回 ok=false")
	}
	if _, ok := RankIC([]float64{1, 2}, []float64{3, 4}); ok {
		t.Error("样本数 <3 应返回 ok=false")
	}
	if _, ok := RankIC([]float64{1, 2, 3}, []float64{1, 2}); ok {
		t.Error("长度不等应返回 ok=false")
	}
}

// TestRankICRobustToOutlier 秩相关应对极端值不敏感（这正是选它而非 Pearson 的理由）。
func TestRankICRobustToOutlier(t *testing.T) {
	f := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	base := []float64{10, 20, 30, 40, 50, 60, 70, 80}
	withOutlier := []float64{10, 20, 30, 40, 50, 60, 70, 8000000}
	ic1, _ := RankIC(f, base)
	ic2, _ := RankIC(f, withOutlier)
	if math.Abs(ic1-ic2) > 1e-9 {
		t.Errorf("放大末尾值不该改变秩相关：%.6f vs %.6f", ic1, ic2)
	}
}

// TestHistoryRoundTrip 历史落盘再读回必须逐点一致（含 gzip/CSV 编解码）。
func TestHistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory()
	dates := []string{"20260901", "20260902", "20260903"}
	for i, d := range dates {
		h.MarkDate(d)
		h.AddBar("600000.SH", d, PricePoint{Close: 10 + float64(i), VolLot: 100 + float64(i), RawClose: 9 + float64(i)})
		h.AddBar("000001.SZ", d, PricePoint{Close: 20 + float64(i), VolLot: 200, RawClose: 19})
		h.AddValuation("600000.SH", d, ValPoint{TurnoverRate: 1.5, PETtm: 8.2, PB: 0.9, CircMvW: 3_000_000})
	}
	if _, err := h.Save(dir); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	got, found, err := Load(dir)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if !found {
		t.Fatal("落盘后应能读回")
	}
	if len(got.Dates) != 3 {
		t.Fatalf("交易日数 = %d，期望 3", len(got.Dates))
	}
	if v := got.Closes["20260902"]["600000.SH"]; math.Abs(v-11) > 1e-3 {
		t.Errorf("读回 close = %v，期望 11", v)
	}
	vp := got.Vals["20260901"]["600000.SH"]
	if math.Abs(vp.PETtm-8.2) > 1e-3 || math.Abs(vp.CircMvW-3_000_000) > 1 {
		t.Errorf("读回估值不符: %+v", vp)
	}
}

// TestLoadMissingDir 目录里没有历史文件时返回 found=false，而不是报错
// （调用方据此提示"先跑 backfill"）。
func TestLoadMissingDir(t *testing.T) {
	_, found, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("缺文件不该报错: %v", err)
	}
	if found {
		t.Error("空目录不该报告找到历史")
	}
}

// TestRunICDetectsKnownSignal 构造一段人造历史，其中"动量"与未来收益严格同向。
// 这是端到端正例：IC 计算器要能从数据里认出这个被植入的信号。
func TestRunICDetectsKnownSignal(t *testing.T) {
	h := syntheticHistory(t, 60, 120)
	cfg := DefaultConfig()
	cfg.MinBars = 20
	cfg.MinCodes = 30
	res, err := RunIC(context.Background(), h, cfg)
	if err != nil {
		t.Fatalf("IC 计算失败: %v", err)
	}
	if res.TradeDates < 20 {
		t.Fatalf("参与截面只有 %d 个，样本太少无法检验", res.TradeDates)
	}
	f, ok := res.Find("momentum", 20)
	if !ok {
		t.Fatal("应有 momentum 的 20 日 IC")
	}
	t.Logf("植入信号的动量因子: %s", f.Describe())
	if f.N < 20 {
		t.Fatalf("momentum IC 样本 %d 太少", f.N)
	}
	// 这是核心断言：数据里植入了"动量正相关"的信号，IC 计算器必须把它认出来（正号）。
	// 若只断言"能算出数"，一个方向算反的实现也能通过 —— 那正是最危险的错误。
	if f.ICMean <= 0.20 {
		t.Errorf("植入的动量信号应产出明显正 IC，实际 IC=%+.4f（方向算反或信号被噪声淹没）", f.ICMean)
	}
}

// TestRunICRejectsEmpty 空历史必须报错而不是静默返回空结果。
func TestRunICRejectsEmpty(t *testing.T) {
	if _, err := RunIC(context.Background(), NewHistory(), DefaultConfig()); err == nil {
		t.Fatal("空历史应报错")
	}
}

// syntheticHistory 造一段人造历史：nCodes 只票、nDates 个交易日。
//
// 植入的信号：每只票有固定"质量"q∈[0,1]，每日收益 = 漂移(q) + 小幅噪声。漂移随 q 递增
// 且 q 在时间上稳定 —— 于是"过去 20 日涨幅"（动量）与未来收益正相关。
// 噪声刻意做小（日振幅 ~0.1%，远小于 q=1 时 20 日累计 4% 的漂移），
// 让植入的信号在 IC 上清晰可见：这是端到端正例，断言的是"方向对"而不只是"能算"。
func syntheticHistory(t *testing.T, nDates, nCodes int) *History {
	t.Helper()
	h := NewHistory()
	dates := make([]string, 0, nDates)
	// 用连续自然日当交易日（IC 只看序列位置，不校验日历真实性）。
	start := 20250101
	for i := 0; i < nDates; i++ {
		d := fmt.Sprintf("%08d", start+i)
		dates = append(dates, d)
		h.MarkDate(d)
	}
	for c := 0; c < nCodes; c++ {
		code := fmt.Sprintf("T%04d.SZ", c)
		q := float64(c) / float64(nCodes-1)
		price := 10.0
		for i, d := range dates {
			// 确定性伪随机噪声（哈希而非 math/rand，保证跨版本可复现），幅度远小于漂移。
			noise := (hashUnit(c, i) - 0.5) * 0.002
			price *= 1 + 0.002*q + noise
			if price < 1 {
				price = 1
			}
			h.AddBar(code, d, PricePoint{Close: price, VolLot: 1000 + float64(c), RawClose: price})
			h.AddValuation(code, d, ValPoint{
				TurnoverRate: 1 + q*3, PETtm: 10 + q*20, PB: 1 + q, CircMvW: 500000 + float64(c)*1000,
			})
		}
	}
	h.Finalize()
	return h
}

// hashUnit 由整数对映射到 [0,1) 的确定性伪随机值（无外部依赖、可复现）。
func hashUnit(a, b int) float64 {
	x := uint64(uint32(a))*2654435761 + uint64(uint32(b))*40503 + 0x9E3779B97F4A7C15
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	return float64(x%1_000_000) / 1_000_000
}

// TestSaveCreatesFiles 落盘必须真的产生两个 gzip 文件（防止 Save 静默不写）。
func TestSaveCreatesFiles(t *testing.T) {
	dir := t.TempDir()
	h := NewHistory()
	h.MarkDate("20260901")
	h.AddBar("600000.SH", "20260901", PricePoint{Close: 10, RawClose: 10})
	if _, err := h.Save(dir); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	for _, name := range []string{"bars.csv.gz", "vals.csv.gz"} {
		p := filepath.Join(dir, name)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("缺文件 %s: %v", name, err)
		}
		if fi.Size() == 0 {
			t.Errorf("%s 是空文件", name)
		}
	}
}

// TestFinalizeIsIdempotent Finalize 必须幂等：它在 Load→RunIC、Run→Save 上会被调用多次，
// 增量追加曾让代码数按调用次数翻倍（回补输出报出两倍标的数）。
func TestFinalizeIsIdempotent(t *testing.T) {
	h := NewHistory()
	for _, d := range []string{"20260901", "20260902"} {
		h.MarkDate(d)
		h.AddBar("600000.SH", d, PricePoint{Close: 10, RawClose: 10})
		h.AddBar("000001.SZ", d, PricePoint{Close: 20, RawClose: 20})
	}
	h.Finalize()
	first := len(h.Codes())
	for i := 0; i < 3; i++ {
		h.Finalize()
		if got := len(h.Codes()); got != first {
			t.Fatalf("第 %d 次 Finalize 后代码数 = %d，期望恒为 %d（非幂等）", i+2, got, first)
		}
	}
	if first != 2 {
		t.Fatalf("代码数 = %d，期望 2", first)
	}
}

// TestRankICTiedRanks 并列值的平均秩：已知夹具断言具体数值，
// 把"并列取最小秩"这类退化改动会被这条测试打掉（此前用变异验证过它确实漏网）。
func TestRankICTiedRanks(t *testing.T) {
	// rf = [0, 1.5, 1.5, 3]，rr = [0, 1, 2, 3] → r = 4.5/sqrt(5.25*5) = 0.928571...
	got, ok := RankIC([]float64{1, 2, 2, 3}, []float64{1, 2, 3, 4})
	if !ok {
		t.Fatal("应可计算")
	}
	const want = 0.9486832980505138
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("并列序列 RankIC = %.10f，期望 %.10f", got, want)
	}
}

// TestWindowDatesEndsAtCrossSection 窗口必须以截面日 T 收尾且长度恰为 n。
// 若窗口不落在 T 上（前视/滞后类错误），这条直接失败。
func TestWindowDatesEndsAtCrossSection(t *testing.T) {
	dates := []string{"d1", "d2", "d3", "d4", "d5"}
	w := windowDates(dates, 3, 3)
	if len(w) != 3 {
		t.Fatalf("窗口长度 = %d，期望 3", len(w))
	}
	if w[len(w)-1] != dates[3] {
		t.Errorf("窗口末元素 = %s，期望截面日 %s（窗口必须以 T 收尾）", w[len(w)-1], dates[3])
	}
	if w[0] != "d2" {
		t.Errorf("窗口首元素 = %s，期望 d2", w[0])
	}
	// 开头不足 n 根时只给可得的部分（调用方据长度判断是否够算）。
	if w := windowDates(dates, 1, 3); len(w) != 2 {
		t.Errorf("开头窗口长度 = %d，期望 2（d1,d2）", len(w))
	}
}

// TestWeightsForKeepsSign 权重建议必须保留 IC 符号（负 IC → 负权重）。
// 改回按 |IC| 归一（评审所称的陷阱）会让这条失败。
func TestWeightsForKeepsSign(t *testing.T) {
	r := Result{ICs: []FactorIC{
		{Name: "momentum", Horizon: 20, N: 100, ICMean: -0.08, ICIR: -0.5},
		{Name: "value", Horizon: 20, N: 100, ICMean: +0.05, ICIR: +0.4},
		{Name: "lowvol", Horizon: 20, N: 100, ICMean: -0.06, ICIR: -0.4},
		{Name: "liquidity", Horizon: 20, N: 100, ICMean: -0.02, ICIR: -0.1}, // 未达门槛
	}}
	w := r.WeightsFor(20)
	if w["momentum"] >= 0 {
		t.Errorf("负 IC 的动量应得负权重，实际 %+.3f", w["momentum"])
	}
	if w["value"] <= 0 {
		t.Errorf("正 IC 的价值应得正权重，实际 %+.3f", w["value"])
	}
	if _, ok := w["liquidity"]; ok {
		t.Errorf("未达门槛的因子不该进建议权重")
	}
	sum := math.Abs(w["momentum"]) + math.Abs(w["value"]) + math.Abs(w["lowvol"])
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("Σ|w| = %.4f，期望 1.0", sum)
	}
}

// TestConfigRejectsMismatchedMinBars MinBars 与生产因子窗口不一致必须拒绝：
// 否则度量的不是选股器真正使用的因子。
func TestConfigRejectsMismatchedMinBars(t *testing.T) {
	h := syntheticHistory(t, 40, 60)
	cfg := DefaultConfig()
	cfg.MinCodes = 30
	cfg.MinBars = 5 // 与生产窗口（20）不符
	if _, err := RunIC(context.Background(), h, cfg); err == nil {
		t.Fatal("MinBars 与生产因子窗口不一致时应报错")
	}
}
