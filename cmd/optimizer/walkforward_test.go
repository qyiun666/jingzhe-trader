package main

// walk-forward 纯逻辑 (切窗 / 衰减率 / 参数漂移 / 加权推荐 / 拼接绩效) 单元测试
// 不依赖数据库与回测引擎, 可直接 go test ./cmd/optimizer

import (
	"math"
	"testing"
	"time"
)

// daysBetween 计算两个 YYYYMMDD 日期之间的天数 (含起点, 用于校验窗口长度)
func daysBetween(t *testing.T, start, end string) int {
	t.Helper()
	s, err := time.Parse(dateLayout, start)
	if err != nil {
		t.Fatalf("日期解析失败: %s", start)
	}
	e, err := time.Parse(dateLayout, end)
	if err != nil {
		t.Fatalf("日期解析失败: %s", end)
	}
	return int(e.Sub(s).Hours()/24) + 1
}

func TestSplitWalkForwardFolds_Basic(t *testing.T) {
	// 2024 是闰年: 20240101 ~ 20241231 共 366 天, 切 3 段每段 122 天
	windows, err := splitWalkForwardFolds("20240101", "20241231", 3)
	if err != nil {
		t.Fatalf("切窗失败: %v", err)
	}
	if len(windows) != 3 {
		t.Fatalf("段数 = %d, 期望 3", len(windows))
	}

	for i, w := range windows {
		if w.Index != i+1 {
			t.Errorf("段 %d 序号 = %d", i, w.Index)
		}
		trainDays := daysBetween(t, w.TrainStart, w.TrainEnd)
		testDays := daysBetween(t, w.TestStart, w.TestEnd)
		// 训练窗约占 2/3, 测试窗约占 1/3
		total := trainDays + testDays
		if math.Abs(float64(trainDays)/float64(total)-2.0/3.0) > 0.02 {
			t.Errorf("段 %d 训练窗占比 %.3f, 期望约 0.667", w.Index, float64(trainDays)/float64(total))
		}
		if testDays < 1 {
			t.Errorf("段 %d 测试窗为空", w.Index)
		}
		// 测试窗紧跟训练窗
		trainEnd, _ := time.Parse(dateLayout, w.TrainEnd)
		if got := trainEnd.AddDate(0, 0, 1).Format(dateLayout); got != w.TestStart {
			t.Errorf("段 %d 测试窗起点 %s 未紧跟训练窗终点 %s", w.Index, w.TestStart, w.TrainEnd)
		}
	}

	// 各段连续且完整覆盖整个区间
	if windows[0].TrainStart != "20240101" {
		t.Errorf("首段起点 = %s, 期望 20240101", windows[0].TrainStart)
	}
	if windows[len(windows)-1].TestEnd != "20241231" {
		t.Errorf("末段终点 = %s, 期望 20241231", windows[len(windows)-1].TestEnd)
	}
	for i := 1; i < len(windows); i++ {
		prevEnd, _ := time.Parse(dateLayout, windows[i-1].TestEnd)
		if got := prevEnd.AddDate(0, 0, 1).Format(dateLayout); got != windows[i].TrainStart {
			t.Errorf("段 %d 起点 %s 未紧跟段 %d 终点 %s",
				windows[i].Index, windows[i].TrainStart, windows[i-1].Index, windows[i-1].TestEnd)
		}
	}
}

func TestSplitWalkForwardFolds_RemainderGoesToLastFold(t *testing.T) {
	// 367 天切 3 段: 122/122/123, 余数归入最后一段
	windows, err := splitWalkForwardFolds("20240101", "20250101", 3)
	if err != nil {
		t.Fatalf("切窗失败: %v", err)
	}
	last := windows[len(windows)-1]
	lastDays := daysBetween(t, last.TrainStart, last.TestEnd)
	firstDays := daysBetween(t, windows[0].TrainStart, windows[0].TestEnd)
	if lastDays != firstDays+1 {
		t.Errorf("末段天数 = %d, 期望 %d (余数归末段)", lastDays, firstDays+1)
	}
}

func TestSplitWalkForwardFolds_Errors(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		folds      int
	}{
		{"结束早于起始", "20241231", "20240101", 3},
		{"起止相同", "20240101", "20240101", 3},
		{"起始格式错误", "2024-01-01", "20241231", 3},
		{"结束格式错误", "20240101", "2024/12/31", 3},
		{"段数为0", "20240101", "20241231", 0},
		{"段数过多", "20240101", "20240110", 5}, // 10 天切 5 段每段不足 3 天
	}
	for _, c := range cases {
		if _, err := splitWalkForwardFolds(c.start, c.end, c.folds); err == nil {
			t.Errorf("%s: 期望报错但未报错", c.name)
		}
	}
}

func TestDecayRate(t *testing.T) {
	// 样本内 2.0 -> 样本外 1.0, 衰减 50%
	if r, ok := decayRate(2.0, 1.0); !ok || math.Abs(r-0.5) > 1e-9 {
		t.Errorf("decayRate(2,1) = %v,%v, 期望 0.5,true", r, ok)
	}
	// 样本外更好时衰减率为负
	if r, ok := decayRate(1.0, 1.5); !ok || math.Abs(r-(-0.5)) > 1e-9 {
		t.Errorf("decayRate(1,1.5) = %v,%v, 期望 -0.5,true", r, ok)
	}
	// 样本内为 0 时无意义
	if _, ok := decayRate(0, 1.0); ok {
		t.Error("decayRate(0,1) 期望 ok=false")
	}
	// 样本内为负时按绝对值归一
	if r, ok := decayRate(-1.0, -2.0); !ok || math.Abs(r-1.0) > 1e-9 {
		t.Errorf("decayRate(-1,-2) = %v,%v, 期望 1.0,true", r, ok)
	}
}

func TestParamDrift(t *testing.T) {
	a := paramKey{short: 5, long: 20, pos: 0.30}
	// 完全相同: 无漂移
	if changed, _ := paramDrift(a, a); changed != 0 {
		t.Errorf("相同参数漂移维度 = %d, 期望 0", changed)
	}
	// 变一个维度
	if changed, _ := paramDrift(a, paramKey{short: 7, long: 20, pos: 0.30}); changed != 1 {
		t.Errorf("单维度漂移 = %d, 期望 1", changed)
	}
	// 三个维度全变
	changed, detail := paramDrift(a, paramKey{short: 3, long: 15, pos: 0.40})
	if changed != 3 {
		t.Errorf("全维度漂移 = %d, 期望 3", changed)
	}
	if detail == "" || detail == "无漂移" {
		t.Errorf("漂移明细不应为空: %q", detail)
	}
}

func TestBestByComposite(t *testing.T) {
	results := []OptResult{
		{ShortPeriod: 3, LongPeriod: 10, Sharpe: 0.5, TotalReturn: 0.05, AnnualReturn: 0.04},
		{ShortPeriod: 5, LongPeriod: 20, Sharpe: 1.5, TotalReturn: 0.20, AnnualReturn: 0.15}, // 三维度全面最优
		{ShortPeriod: 7, LongPeriod: 25, Sharpe: 1.0, TotalReturn: 0.10, AnnualReturn: 0.08},
	}
	best, ok := bestByComposite(results)
	if !ok {
		t.Fatal("bestByComposite 返回 ok=false")
	}
	if best.ShortPeriod != 5 || best.LongPeriod != 20 {
		t.Errorf("最优参数 = %d/%d, 期望 5/20", best.ShortPeriod, best.LongPeriod)
	}
	// 空输入
	if _, ok := bestByComposite(nil); ok {
		t.Error("空输入期望 ok=false")
	}
}

func TestOOSWeight(t *testing.T) {
	// 正夏普 + 正收益: 取两者之和
	if w := oosWeight(OptResult{Sharpe: 1.0, TotalReturn: 0.10}); math.Abs(w-1.1) > 1e-9 {
		t.Errorf("oosWeight = %v, 期望 1.1", w)
	}
	// 全负: 权重为 0
	if w := oosWeight(OptResult{Sharpe: -0.5, TotalReturn: -0.10}); w != 0 {
		t.Errorf("全负 oosWeight = %v, 期望 0", w)
	}
	// 一正一负: 只取正部
	if w := oosWeight(OptResult{Sharpe: 0.8, TotalReturn: -0.05}); math.Abs(w-0.8) > 1e-9 {
		t.Errorf("一正一负 oosWeight = %v, 期望 0.8", w)
	}
}

func TestRecommendParam(t *testing.T) {
	// 参数 A 在两段被选中且样本外表现为正, 参数 B 一段但权重更高 -> B 胜出
	folds := []foldResult{
		{Window: WFWindow{Index: 1}, OOS: OptResult{ShortPeriod: 5, LongPeriod: 20, PositionPct: 0.30, Sharpe: 0.5, TotalReturn: 0.05}},
		{Window: WFWindow{Index: 2}, OOS: OptResult{ShortPeriod: 5, LongPeriod: 20, PositionPct: 0.30, Sharpe: 0.4, TotalReturn: 0.04}},
		{Window: WFWindow{Index: 3}, OOS: OptResult{ShortPeriod: 3, LongPeriod: 15, PositionPct: 0.25, Sharpe: 1.2, TotalReturn: 0.15}},
	}
	best, _, ok := recommendParam(folds)
	if !ok {
		t.Fatal("recommendParam 返回 ok=false")
	}
	// A 权重 0.55+0.44=0.99, B 权重 1.35 -> 推荐 B (3/15)
	if best.short != 3 || best.long != 15 {
		t.Errorf("推荐参数 = %d/%d, 期望 3/15 (样本外权重最高)", best.short, best.long)
	}

	// 权重全 0 (样本外全部亏损): 退化为样本外得分最高者
	losing := []foldResult{
		{Window: WFWindow{Index: 1}, OOS: OptResult{ShortPeriod: 5, LongPeriod: 20, Sharpe: -0.5, TotalReturn: -0.10}},
		{Window: WFWindow{Index: 2}, OOS: OptResult{ShortPeriod: 7, LongPeriod: 25, Sharpe: -0.1, TotalReturn: -0.02}},
	}
	best, _, ok = recommendParam(losing)
	if !ok || best.short != 7 {
		t.Errorf("全亏兜底推荐 = %d/%d,%v, 期望 7/25,true", best.short, best.long, ok)
	}

	// 空输入
	if _, _, ok := recommendParam(nil); ok {
		t.Error("空输入期望 ok=false")
	}
}

func TestStitchOOS(t *testing.T) {
	folds := []foldResult{
		{
			Window: WFWindow{Index: 1, TestStart: "20240401", TestEnd: "20240630"}, // 91 天
			OOS:    OptResult{TotalReturn: 0.10, Sharpe: 1.0, MaxDrawdown: 0.05},
		},
		{
			Window: WFWindow{Index: 2, TestStart: "20241001", TestEnd: "20241231"}, // 92 天
			OOS:    OptResult{TotalReturn: 0.20, Sharpe: 2.0, MaxDrawdown: 0.08},
		},
	}
	st := stitchOOS(folds)
	// 总收益复利连乘: 1.1 * 1.2 - 1 = 0.32
	if math.Abs(st.TotalReturn-0.32) > 1e-9 {
		t.Errorf("拼接总收益 = %v, 期望 0.32", st.TotalReturn)
	}
	// 夏普按天数加权: (1.0*91 + 2.0*92) / 183
	wantSharpe := (1.0*91 + 2.0*92) / 183
	if math.Abs(st.Sharpe-wantSharpe) > 1e-9 {
		t.Errorf("拼接夏普 = %v, 期望 %v", st.Sharpe, wantSharpe)
	}
	// 回撤取各段最大
	if math.Abs(st.MaxDrawdown-0.08) > 1e-9 {
		t.Errorf("拼接回撤 = %v, 期望 0.08", st.MaxDrawdown)
	}
}

func TestParamGridCombos(t *testing.T) {
	grid := paramGrid{
		ShortPeriods: []int{3, 5, 7, 10},
		LongPeriods:  []int{10, 15, 20, 25, 30},
		PositionPcts: []float64{0.25, 0.30, 0.35, 0.40},
	}
	combos := grid.combos()
	// 有效 (short,long) 对: short=3/5/7 各 5 个, short=10 时 4 个 (剔除 long=10) -> 19 对 x 4 仓位
	if len(combos) != 19*4 {
		t.Errorf("有效组合数 = %d, 期望 %d", len(combos), 19*4)
	}
	// 所有组合必须满足 short < long
	for _, c := range combos {
		sp, lp := c[0].(int), c[1].(int)
		if sp >= lp {
			t.Errorf("出现 short>=long 组合: %d/%d", sp, lp)
		}
	}
}
