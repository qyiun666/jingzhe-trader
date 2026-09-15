package screener

import (
	"math"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestWeightsByModeReversalFlipsSign 反向模式必须把三条被数据否定的因子翻成负号，
// 且动量这条主要病灶确实由正转负 —— 这是"修好选股方向"的最小可验证单元。
func TestWeightsByModeReversalFlipsSign(t *testing.T) {
	d := WeightsByMode(ModeMomentum)
	if d.Momentum != 0.30 || d.Value != 0.25 || d.LowVol != 0.20 || d.Liquidity != 0.25 {
		t.Fatalf("momentum 模式应保持原权重，实际 %+v", d)
	}
	r := WeightsByMode(ModeReversal)
	if r.Momentum >= 0 {
		t.Errorf("反向模式下动量权重应为负，实际 %.2f", r.Momentum)
	}
	if r.LowVol >= 0 {
		t.Errorf("反向模式下低波权重应为负，实际 %.2f", r.LowVol)
	}
	if r.Liquidity >= 0 {
		t.Errorf("反向模式下流动性权重应为负，实际 %.2f", r.Liquidity)
	}
	if r.Value <= 0 {
		t.Errorf("价值因子 IC 为正，反向模式下应保持正权重，实际 %.2f", r.Value)
	}
	// 兜底与 config 键目录的默认值一致（reversal），不是任意一侧。
	// 非法取值由装配期 validateEnums 与 research ic 各自 fail-closed 拦下。
	if got := WeightsByMode(""); got != ReversalWeights() {
		t.Errorf("空模式应回落 reversal（与默认值一致），实际 %+v", got)
	}
	if got := WeightsByMode("bogus"); got != ReversalWeights() {
		t.Errorf("未知模式应回落 reversal（与默认值一致），实际 %+v", got)
	}
}

// TestDefaultFactorModeIsReversal 键目录默认值与 WeightsByMode 的兜底必须同向：
// 两处一旦分叉，"生产在用哪个方向"就没有唯一答案。
func TestDefaultFactorModeIsReversal(t *testing.T) {
	// 与 internal/config/keys.go 的 screen.factor_mode 默认值保持同一口径。
	const keyDefault = "reversal"
	if !ValidFactorMode(keyDefault) {
		t.Fatalf("%s 不是合法模式", keyDefault)
	}
	if got := WeightsByMode(keyDefault); got != ReversalWeights() {
		t.Errorf("键目录默认值 %s 应映射到 ReversalWeights，实际 %+v", keyDefault, got)
	}
}

// TestReversalWeightsNormalized 反向权重按 Σ|w| = 1 归一（便于与旧权重直接对照）。
func TestReversalWeightsNormalized(t *testing.T) {
	w := ReversalWeights()
	sum := math.Abs(w.Momentum) + math.Abs(w.Value) + math.Abs(w.LowVol) + math.Abs(w.Liquidity)
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("Σ|w| = %.4f，期望 1.0", sum)
	}
}

// TestCompositeDirectionMatters 用一个具体截面演示方向影响：
// 同一组因子分，两套权重给出**相反**的综合分排序。
// 这是 IC 结论的微观镜像：权重定反了，选出来的就是完全另一批票。
func TestCompositeDirectionMatters(t *testing.T) {
	// A：动量/低波/流动性都极高、价值极低（原权重眼里的"好票"）。
	a := model.FactorScore{Momentum: 95, Value: 5, LowVol: 90, Liquidity: 95}
	// B：动量/低波/流动性都极低、价值很高（反向权重眼里的"好票"）。
	b := model.FactorScore{Momentum: 5, Value: 95, LowVol: 10, Liquidity: 5}

	oldA, oldB := Composite(a, DefaultWeights()), Composite(b, DefaultWeights())
	if oldA <= oldB {
		t.Fatalf("原权重下 A(追热) 应排在 B 前面，实际 A=%.1f B=%.1f", oldA, oldB)
	}
	revA, revB := Composite(a, ReversalWeights()), Composite(b, ReversalWeights())
	if revA >= revB {
		t.Errorf("反向权重下排序应翻转（B 在前），实际 A=%.1f B=%.1f", revA, revB)
	}
}

// TestHardFiltersMatchesProductionStages HardFilters 是全部 IC 数字的"可投池"定义，
// 它必须与生产漏斗的流动性/估值两级判定一致。漏掉任一级会让 IC 静默漂移。
func TestHardFiltersMatchesProductionStages(t *testing.T) {
	cfg := FilterConfig{MinCircMvW: 500000, MinTurnoverRate: 1.0, PriceLow: 2.0, PETtmMax: 80, PBMax: 10}
	price := model.FromFloat(10)
	good := model.StockBasic{CircMvW: 600000, TurnoverRate: 1.5, PETtm: 20, PB: 2}
	if !HardFilters(good, price, cfg) {
		t.Error("全部达标的票应通过")
	}
	cases := []struct {
		name  string
		mut   func(*model.StockBasic)
		price model.Fen
	}{
		{"流通市值不足", func(s *model.StockBasic) { s.CircMvW = 400000 }, price},
		{"换手不足", func(s *model.StockBasic) { s.TurnoverRate = 0.5 }, price},
		{"价格过低", func(s *model.StockBasic) {}, model.FromFloat(1.5)},
		{"亏损(PE<=0)", func(s *model.StockBasic) { s.PETtm = -1 }, price},
		{"PE 超标", func(s *model.StockBasic) { s.PETtm = 200 }, price},
		{"PB 超标", func(s *model.StockBasic) { s.PB = 30 }, price},
	}
	for _, c := range cases {
		s := good
		c.mut(&s)
		if HardFilters(s, c.price, cfg) {
			t.Errorf("%s：应被剔除，实际通过", c.name)
		}
	}
}

// TestTopFactorNamesFollowsWeightSign 反向模式下理由文案不能把被反向的因子列为亮点。
func TestTopFactorNamesFollowsWeightSign(t *testing.T) {
	// 动量/低波/流动性都很高、价值很低：原权重下它们是亮点。
	fs := model.FactorScore{Momentum: 95, Value: 5, LowVol: 90, Liquidity: 95}
	rev := ReversalWeights()
	got := topFactorNames(fs, rev)
	// 反向模式下三项权重为负，价值（正权重×低分）贡献最大 —— 文案应体现价值而非动量。
	if !strings.Contains(got, "价值") {
		t.Errorf("反向模式下理由应体现正权重因子（价值），实际 %q", got)
	}
	if strings.Contains(got, "动量") || strings.Contains(got, "流动性") {
		t.Errorf("反向模式下不该把负权重因子列为亮点，实际 %q", got)
	}
}
