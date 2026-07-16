package factor

import (
	"context"
	"math"
	"testing"

	"jingzhe-trader/internal/model"
)

// --- 测试 Winsorize ---

func TestWinsorize_Basic(t *testing.T) {
	// 使用更多数据点, 确保5%/95%分位数落在极值上
	values := make([]float64, 100)
	for i := 0; i < 99; i++ {
		values[i] = float64(i + 1) // 1, 2, 3, ..., 99
	}
	values[99] = 1000.0 // 一个极端大值

	result := Winsorize(values, 0.05, 0.95)

	// 最大值1000应该被缩尾 (值变小)
	if result[99] >= 1000 {
		t.Errorf("上侧极值应该被缩尾, got %v", result[99])
	}

	// 最小值1应该被缩尾 (值变大)
	if result[0] <= values[0] {
		t.Errorf("下侧极值应该被缩尾(值变大), got %v", result[0])
	}

	// 中间值应该保持不变 (50%位置的值)
	if result[50] != values[50] {
		t.Errorf("中间值应该保持不变, expected=%v, got=%v", values[50], result[50])
	}
}

func TestWinsorize_Empty(t *testing.T) {
	result := Winsorize([]float64{}, 0.05, 0.95)
	if len(result) != 0 {
		t.Errorf("空切片应该返回空切片, got %v", result)
	}
}

func TestWinsorize_SingleValue(t *testing.T) {
	values := []float64{5.0}
	result := Winsorize(values, 0.05, 0.95)
	if len(result) != 1 || result[0] != 5.0 {
		t.Errorf("单元素应该保持不变, got %v", result)
	}
}

func TestWinsorize_LowerBound(t *testing.T) {
	values := []float64{-100, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	result := Winsorize(values, 0.1, 0.9)

	// -100 应该被缩尾到更低的分位数值
	if result[0] <= -100 {
		t.Errorf("下侧极值应该被缩尾, got %v", result[0])
	}
}

func TestWinsorize_InvalidBounds(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	// lower >= upper 应该原样返回
	result := Winsorize(values, 0.5, 0.5)
	for i, v := range values {
		if result[i] != v {
			t.Errorf("无效边界应该原样返回, index=%d", i)
		}
	}
}

// --- 测试 Standardize ---

func TestStandardize_Basic(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	result := Standardize(values)

	// 标准化后均值应为0
	var sum float64
	for _, v := range result {
		sum += v
	}
	mean := sum / float64(len(result))
	if math.Abs(mean) > 1e-10 {
		t.Errorf("标准化后均值应该接近0, got %v", mean)
	}

	// 标准差应为1
	var variance float64
	for _, v := range result {
		variance += (v - mean) * (v - mean)
	}
	variance /= float64(len(result))
	std := math.Sqrt(variance)
	if math.Abs(std-1.0) > 1e-10 {
		t.Errorf("标准化后标准差应该接近1, got %v", std)
	}
}

func TestStandardize_Empty(t *testing.T) {
	result := Standardize([]float64{})
	if len(result) != 0 {
		t.Errorf("空切片应该返回空切片, got %v", result)
	}
}

func TestStandardize_AllSame(t *testing.T) {
	values := []float64{5, 5, 5, 5, 5}
	result := Standardize(values)

	// 所有值相同时, 标准差为0, 应返回全0
	for i, v := range result {
		if v != 0 {
			t.Errorf("所有值相同时应返回0, index=%d, got=%v", i, v)
		}
	}
}

func TestStandardize_SingleValue(t *testing.T) {
	values := []float64{10.0}
	result := Standardize(values)
	if len(result) != 1 || result[0] != 0 {
		t.Errorf("单元素应该返回0, got %v", result)
	}
}

// --- 测试 Rank ---

func TestRank_HigherBetter(t *testing.T) {
	values := []float64{10, 30, 20, 50, 40}
	result := Rank(values, true)

	// 最大值50 (索引3) 应该得100分
	if result["3"] != 100.0 {
		t.Errorf("最大值应该得100分, got %v", result["3"])
	}

	// 最小值10 (索引0) 应该得0分
	if result["0"] != 0.0 {
		t.Errorf("最小值应该得0分, got %v", result["0"])
	}

	// 第二大值40 (索引4) 应该得75分 (100 * (1 - 1/4) = 75)
	if math.Abs(result["4"]-75.0) > 1e-10 {
		t.Errorf("第二大值应该得75分, got %v", result["4"])
	}
}

func TestRank_LowerBetter(t *testing.T) {
	values := []float64{10, 30, 20, 50, 40}
	result := Rank(values, false)

	// 最小值10 (索引0) 应该得100分
	if result["0"] != 100.0 {
		t.Errorf("最小值应该得100分, got %v", result["0"])
	}

	// 最大值50 (索引3) 应该得0分
	if result["3"] != 0.0 {
		t.Errorf("最大值应该得0分, got %v", result["3"])
	}
}

func TestRank_Empty(t *testing.T) {
	result := Rank([]float64{}, true)
	if len(result) != 0 {
		t.Errorf("空切片应该返回空map, got %v", result)
	}
}

func TestRank_SingleValue(t *testing.T) {
	result := Rank([]float64{5.0}, true)
	if len(result) != 1 {
		t.Errorf("单元素应该返回1个元素, got %v", result)
	}
	// 单元素应该得50分 (中间值)
	if result["0"] != 50.0 {
		t.Errorf("单元素应该得50分, got %v", result["0"])
	}
}

// --- Mock DataProvider 用于因子测试 ---

// mockDataProvider 模拟数据提供者
type mockDataProvider struct {
	dailyBasics   map[string][]model.DailyBasic   // date -> []DailyBasic
	finaIndicator map[string][]model.FinaIndicator // tsCode -> []FinaIndicator
	stocks        map[string]*model.Stock          // tsCode -> Stock
	bars          map[string][]model.Bar           // tsCode -> []Bar
}

func (m *mockDataProvider) GetDailyBasic(date string) ([]model.DailyBasic, error) {
	if m.dailyBasics == nil {
		return nil, nil
	}
	return m.dailyBasics[date], nil
}

func (m *mockDataProvider) GetDailyBasicByCode(tsCode, startDate, endDate string) ([]model.DailyBasic, error) {
	if m.dailyBasics == nil {
		return nil, nil
	}
	// 模拟: 从 dailyBasics 中按日期范围过滤
	var result []model.DailyBasic
	for _, basics := range m.dailyBasics {
		for _, b := range basics {
			if b.TsCode == tsCode && b.TradeDate >= startDate && b.TradeDate <= endDate {
				result = append(result, b)
			}
		}
	}
	return result, nil
}

func (m *mockDataProvider) GetFinaIndicator(tsCode string) ([]model.FinaIndicator, error) {
	if m.finaIndicator == nil {
		return nil, nil
	}
	return m.finaIndicator[tsCode], nil
}

func (m *mockDataProvider) GetStockByCode(tsCode string) (*model.Stock, error) {
	if m.stocks == nil {
		return nil, nil
	}
	return m.stocks[tsCode], nil
}

func (m *mockDataProvider) GetBars(tsCode, startDate, endDate string) ([]model.Bar, error) {
	if m.bars == nil {
		return nil, nil
	}
	return m.bars[tsCode], nil
}

// --- 测试价值因子 ---

func TestPEFactor(t *testing.T) {
	provider := &mockDataProvider{
		dailyBasics: map[string][]model.DailyBasic{
			"20240101": {
				{TsCode: "000001.SZ", PE_TTM: 10.0},
				{TsCode: "000002.SZ", PE_TTM: 20.0},
				{TsCode: "000003.SZ", PE_TTM: -5.0}, // 亏损
				{TsCode: "000004.SZ", PE_TTM: 5.0},
			},
		},
	}

	factor := &PEFactor{}
	universe := []string{"000001.SZ", "000002.SZ", "000003.SZ", "000004.SZ"}
	result, err := factor.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	if len(result) != 4 {
		t.Errorf("应该返回4个结果, got %d", len(result))
	}

	// PE越低越好, 所以 -PE 越大越好
	// PE=5 应该 > PE=10 应该 > PE=20
	if result["000004.SZ"] <= result["000001.SZ"] {
		t.Errorf("PE=5的分数应该高于PE=10")
	}
	if result["000001.SZ"] <= result["000002.SZ"] {
		t.Errorf("PE=10的分数应该高于PE=20")
	}

	// 亏损股PE为负, 应该得最低分
	if !math.IsInf(result["000003.SZ"], -1) {
		t.Errorf("亏损股应该得负无穷分, got %v", result["000003.SZ"])
	}
}

func TestPBFactor(t *testing.T) {
	provider := &mockDataProvider{
		dailyBasics: map[string][]model.DailyBasic{
			"20240101": {
				{TsCode: "000001.SZ", PB: 1.5},
				{TsCode: "000002.SZ", PB: 3.0},
				{TsCode: "000003.SZ", PB: 0.8},
			},
		},
	}

	factor := &PBFactor{}
	universe := []string{"000001.SZ", "000002.SZ", "000003.SZ"}
	result, err := factor.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	if len(result) != 3 {
		t.Errorf("应该返回3个结果, got %d", len(result))
	}

	// PB越低越好
	if result["000003.SZ"] <= result["000001.SZ"] {
		t.Errorf("PB=0.8的分数应该高于PB=1.5")
	}
}

func TestDividendYieldFactor(t *testing.T) {
	provider := &mockDataProvider{
		dailyBasics: map[string][]model.DailyBasic{
			"20240101": {
				{TsCode: "000001.SZ", DV_RATIO: 5.0},
				{TsCode: "000002.SZ", DV_RATIO: 2.0},
				{TsCode: "000003.SZ", DV_RATIO: 8.0},
			},
		},
	}

	factor := &DividendYieldFactor{}
	universe := []string{"000001.SZ", "000002.SZ", "000003.SZ"}
	result, err := factor.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	if result["000003.SZ"] != 8.0 {
		t.Errorf("股息率应该等于DV_RATIO, got %v", result["000003.SZ"])
	}
}

// --- 测试质量因子 ---

func TestROEFactor(t *testing.T) {
	provider := &mockDataProvider{
		finaIndicator: map[string][]model.FinaIndicator{
			"000001.SZ": {
				{TsCode: "000001.SZ", AnnDate: "20231030", EndDate: "20230930", ROE: 15.0},
				{TsCode: "000001.SZ", AnnDate: "20240430", EndDate: "20231231", ROE: 18.0},
			},
			"000002.SZ": {
				{TsCode: "000002.SZ", AnnDate: "20231030", EndDate: "20230930", ROE: 10.0},
			},
		},
	}

	factor := &ROEFactor{}
	universe := []string{"000001.SZ", "000002.SZ"}

	// 在20240101, 000001的最近财报是20230930 (ROE=15), 因为20231231还没公告
	result, err := factor.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	if result["000001.SZ"] != 15.0 {
		t.Errorf("ROE应该为15.0, got %v", result["000001.SZ"])
	}
	if result["000002.SZ"] != 10.0 {
		t.Errorf("ROE应该为10.0, got %v", result["000002.SZ"])
	}
}

func TestDebtToAssetsFactor(t *testing.T) {
	provider := &mockDataProvider{
		finaIndicator: map[string][]model.FinaIndicator{
			"000001.SZ": {
				{TsCode: "000001.SZ", AnnDate: "20231030", EndDate: "20230930", DebtToAssets: 60.0},
			},
			"000002.SZ": {
				{TsCode: "000002.SZ", AnnDate: "20231030", EndDate: "20230930", DebtToAssets: 40.0},
			},
		},
	}

	factor := &DebtToAssetsFactor{}
	universe := []string{"000001.SZ", "000002.SZ"}
	result, err := factor.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	// 资产负债率越低越好, 返回负值
	// 负债率40% 的公司应该得分更高 (-40 > -60)
	if result["000002.SZ"] <= result["000001.SZ"] {
		t.Errorf("负债率低的公司得分应该更高")
	}
}

// --- 测试成长因子 ---

func TestNetProfitYoyFactor(t *testing.T) {
	provider := &mockDataProvider{
		finaIndicator: map[string][]model.FinaIndicator{
			"000001.SZ": {
				{TsCode: "000001.SZ", AnnDate: "20231030", EndDate: "20230930", NetProfitYoy: 20.0},
			},
		},
	}

	factor := &NetProfitYoyFactor{}
	universe := []string{"000001.SZ"}
	result, err := factor.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	if result["000001.SZ"] != 20.0 {
		t.Errorf("净利润同比应该为20.0, got %v", result["000001.SZ"])
	}
}

func TestRevenueYoyFactor(t *testing.T) {
	provider := &mockDataProvider{
		finaIndicator: map[string][]model.FinaIndicator{
			"000001.SZ": {
				{TsCode: "000001.SZ", AnnDate: "20231030", EndDate: "20230930", ORYoy: 15.0},
			},
		},
	}

	factor := &RevenueYoyFactor{}
	universe := []string{"000001.SZ"}
	result, err := factor.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	if result["000001.SZ"] != 15.0 {
		t.Errorf("营收同比应该为15.0, got %v", result["000001.SZ"])
	}
}

// --- 测试动量因子 ---

func TestMomentumFactor(t *testing.T) {
	// 构造20天的K线数据, 价格从100涨到120
	bars := make([]model.Bar, 20)
	for i := 0; i < 20; i++ {
		date := 20240101 + i
		price := 100.0 + float64(i) // 每天涨1元
		bars[i] = model.Bar{
			TsCode:    "000001.SZ",
			TradeDate: itoa(date),
			Close:     price,
			AdjFactor: 1.0,
		}
	}

	provider := &mockDataProvider{
		bars: map[string][]model.Bar{
			"000001.SZ": bars,
		},
	}

	factor := &MomentumFactor{Period: 10}
	universe := []string{"000001.SZ"}
	result, err := factor.Compute(context.Background(), "20240120", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	// 过去10天从110涨到120, 涨幅约 10/110 = 9.09%
	if result["000001.SZ"] <= 0 {
		t.Errorf("动量应该为正, got %v", result["000001.SZ"])
	}
}

// --- 测试多因子合成 ---

func TestCompositeFactor(t *testing.T) {
	provider := &mockDataProvider{
		dailyBasics: map[string][]model.DailyBasic{
			"20240101": {
				{TsCode: "000001.SZ", PE_TTM: 10.0, PB: 1.5, DV_RATIO: 3.0},
				{TsCode: "000002.SZ", PE_TTM: 20.0, PB: 2.5, DV_RATIO: 5.0},
				{TsCode: "000003.SZ", PE_TTM: 15.0, PB: 2.0, DV_RATIO: 4.0},
			},
		},
	}

	factors := []FactorWeight{
		{Factor: &PEFactor{}, Weight: 0.5},
		{Factor: &DividendYieldFactor{}, Weight: 0.5},
	}

	composite := NewCompositeFactor("value_composite", factors)
	universe := []string{"000001.SZ", "000002.SZ", "000003.SZ"}

	results, err := composite.Compute(context.Background(), "20240101", universe, provider)
	if err != nil {
		t.Fatalf("Compute failed: %v", err)
	}

	if len(results) != 3 {
		t.Errorf("应该返回3个结果, got %d", len(results))
	}

	// 结果应该按得分降序排列
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("结果应该按得分降序排列")
		}
	}

	// 每个结果都应该包含所有因子的原始值
	for _, r := range results {
		if _, ok := r.Factors["pe_ttm"]; !ok {
			t.Errorf("结果应该包含pe_ttm因子值")
		}
		if _, ok := r.Factors["dividend_yield"]; !ok {
			t.Errorf("结果应该包含dividend_yield因子值")
		}
	}
}

func TestSelectTopN(t *testing.T) {
	results := []CompositeResult{
		{TsCode: "000001.SZ", Score: 90.0},
		{TsCode: "000002.SZ", Score: 80.0},
		{TsCode: "000003.SZ", Score: 70.0},
		{TsCode: "000004.SZ", Score: 60.0},
		{TsCode: "000005.SZ", Score: 50.0},
	}

	// 取前3名
	top3 := SelectTopN(results, 3)
	if len(top3) != 3 {
		t.Errorf("应该返回3个结果, got %d", len(top3))
	}
	if top3[0].TsCode != "000001.SZ" {
		t.Errorf("第一名应该是000001.SZ, got %s", top3[0].TsCode)
	}

	// N大于总数时返回全部
	top10 := SelectTopN(results, 10)
	if len(top10) != 5 {
		t.Errorf("N大于总数时应该返回全部, got %d", len(top10))
	}

	// N<=0时返回nil
	top0 := SelectTopN(results, 0)
	if top0 != nil {
		t.Errorf("N<=0时应该返回nil, got %v", top0)
	}
}

// --- 测试 quantile 辅助函数 ---

func TestQuantile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	// 中位数
	median := quantile(sorted, 0.5)
	if math.Abs(median-5.5) > 1e-10 {
		t.Errorf("中位数应该是5.5, got %v", median)
	}

	// 最小值
	minVal := quantile(sorted, 0.0)
	if minVal != 1.0 {
		t.Errorf("最小值应该是1.0, got %v", minVal)
	}

	// 最大值
	maxVal := quantile(sorted, 1.0)
	if maxVal != 10.0 {
		t.Errorf("最大值应该是10.0, got %v", maxVal)
	}
}
