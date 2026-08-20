package screener

import (
	"math"
	"testing"

	"jingzhe-trader/internal/store"
)

// TestCalcTrend 满5日数据时 MA 为最近5日均价, 动量基于最远一日
func TestCalcTrend(t *testing.T) {
	// recentCloses 从近到远: [d-1, d-2, d-3, d-4, d-5]
	recent := []float64{110, 108, 106, 104, 100}
	ma5, momentum, days := calcTrend(recent, 112)
	if days != 5 {
		t.Errorf("days 应为 5, 实际 %d", days)
	}
	wantMA := (110 + 108 + 106 + 104 + 100) / 5.0
	if math.Abs(ma5-wantMA) > 1e-9 {
		t.Errorf("MA5 应为 %.2f, 实际 %.2f", wantMA, ma5)
	}
	// 动量 = (112-100)/100*100 = 12
	if math.Abs(momentum-12) > 1e-9 {
		t.Errorf("动量应为 12, 实际 %.2f", momentum)
	}
}

// TestCalcTrendInsufficient 数据不足5日时按实际天数计算, 动量仍基于最远一日
func TestCalcTrendInsufficient(t *testing.T) {
	recent := []float64{110, 105} // 2 日
	ma5, momentum, days := calcTrend(recent, 112)
	if days != 2 {
		t.Errorf("days 应为 2, 实际 %d", days)
	}
	wantMA := (110 + 105) / 2.0
	if math.Abs(ma5-wantMA) > 1e-9 {
		t.Errorf("MA 应为 %.2f, 实际 %.2f", wantMA, ma5)
	}
	wantMomentum := (112.0 - 105) / 105 * 100
	if math.Abs(momentum-wantMomentum) > 1e-9 {
		t.Errorf("动量应为 %.2f, 实际 %.2f", wantMomentum, momentum)
	}
}

// TestCalcTrendEmpty 空输入时全部返回 0
func TestCalcTrendEmpty(t *testing.T) {
	ma5, momentum, days := calcTrend(nil, 112)
	if ma5 != 0 || momentum != 0 || days != 0 {
		t.Errorf("空输入应返回 0,0,0, 实际 %.2f %.2f %d", ma5, momentum, days)
	}
}

// TestDiversifyByIndustryQuota 同行业不超过 maxPerIndustry 只, 候选未超配额时全部入选
func TestDiversifyByIndustryQuota(t *testing.T) {
	cands := []store.ScreenResult{
		{TsCode: "600519.SH", Score: 5}, // 白酒
		{TsCode: "000858.SZ", Score: 4}, // 白酒
		{TsCode: "601318.SH", Score: 3}, // 保险
		{TsCode: "600036.SH", Score: 2}, // 银行
	}
	industries := map[string]string{
		"600519.SH": "白酒", "000858.SZ": "白酒",
		"601318.SH": "保险", "600036.SH": "银行",
	}
	got := diversifyByIndustry(cands, industries, 10, 2)
	if len(got) != 4 {
		t.Fatalf("应全部入选(白酒2只未超配额), 实际 %d", len(got))
	}
}

// TestDiversifyByIndustryLimit 行业集中时不超过配额, 不撤销分散约束
func TestDiversifyByIndustryLimit(t *testing.T) {
	cands := []store.ScreenResult{
		{TsCode: "600519.SH", Score: 5}, // 白酒
		{TsCode: "000858.SZ", Score: 4}, // 白酒
		{TsCode: "000568.SZ", Score: 3}, // 白酒
		{TsCode: "600809.SH", Score: 2}, // 白酒
		{TsCode: "601318.SH", Score: 1}, // 保险
	}
	industries := map[string]string{
		"600519.SH": "白酒", "000858.SZ": "白酒",
		"000568.SZ": "白酒", "600809.SH": "白酒",
		"601318.SH": "保险",
	}
	got := diversifyByIndustry(cands, industries, 10, 2)
	if len(got) != 3 {
		t.Fatalf("白酒最多2只+保险1只=3, 实际 %d", len(got))
	}
	// 白酒入选的应是分数最高的两只
	if got[0].TsCode != "600519.SH" || got[1].TsCode != "000858.SZ" {
		t.Errorf("白酒应选分数最高的两只, 实际 %s %s", got[0].TsCode, got[1].TsCode)
	}
}

// TestDiversifyByIndustryMaxN TopN 截断
func TestDiversifyByIndustryMaxN(t *testing.T) {
	cands := []store.ScreenResult{
		{TsCode: "600519.SH", Score: 5},
		{TsCode: "601318.SH", Score: 4},
		{TsCode: "600036.SH", Score: 3},
	}
	industries := map[string]string{"600519.SH": "白酒", "601318.SH": "保险", "600036.SH": "银行"}
	got := diversifyByIndustry(cands, industries, 2, 1)
	if len(got) != 2 {
		t.Fatalf("应截断为 2, 实际 %d", len(got))
	}
}

// TestDiversifyByIndustryUnknown 无行业信息(含 nil 映射)时回退 unknown 分组并受配额约束
func TestDiversifyByIndustryUnknown(t *testing.T) {
	cands := []store.ScreenResult{
		{TsCode: "600519.SH", Score: 5},
		{TsCode: "000858.SZ", Score: 4},
		{TsCode: "601318.SH", Score: 3},
	}
	got := diversifyByIndustry(cands, nil, 10, 1)
	if len(got) != 1 {
		t.Fatalf("无行业信息应全部归入 unknown 且最多1只, 实际 %d", len(got))
	}
}
