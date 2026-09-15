package tushare

import (
	"math"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestToModelValuationCircMvUnit 流通市值单位回归。
//
// 用 2026-09-15 从 Tushare daily_basic 实拉的原始值断言：circ_mv 的单位是**万元**。
// 原实现按"千元"再除以 10，把每个市值都缩小 10 倍（门槛实际变严 10 倍、喂给 LLM 的
// 市值也小 10 倍）。这条测试用真实返回值和已知市值交叉验证，是那次错误的回归锚点。
func TestToModelValuationCircMvUnit(t *testing.T) {
	cases := []struct {
		name        string
		raw         float64
		wantWanYuan float64 // 期望的万元口径值
		wantYi      float64 // 期望的亿元（仅用于人读断言）
	}{
		{"平安银行 000001.SZ", 22859896.93, 22859896.93, 2286},
		{"贵州茅台 600519.SH", 163673183.888, 163673183.888, 16367},
		{"桐昆股份 601233.SH", 5953303.2054, 5953303.2054, 595},
	}
	for _, c := range cases {
		got := ToModelValuation(RawValuation{TsCode: "X", CircMv: c.raw})
		if math.Abs(got.CircMvW-c.wantWanYuan) > 1 {
			t.Errorf("%s: CircMvW = %.2f 万元，期望 %.2f（原始 %.2f 即万元口径，不该再换算）",
				c.name, got.CircMvW, c.wantWanYuan, c.raw)
		}
		if yi := got.CircMvW / 10000; math.Abs(yi-c.wantYi) > 1 {
			t.Errorf("%s: %.0f 万元 = %.0f 亿，期望 %.0f 亿", c.name, got.CircMvW, yi, c.wantYi)
		}
		// 显式反例：旧实现（除以 10）会把市值缩到 1/10，这条断言锁死它不再回来。
		if math.Abs(got.CircMvW-c.raw/10.0) < 1 {
			t.Errorf("%s: 值等于 raw/10，说明按千元换算的错误实现又回来了", c.name)
		}
	}
}

// TestToModelValuationOtherFields 其余字段口径不变（换手率 % / PE / PB 原样透传）。
func TestToModelValuationOtherFields(t *testing.T) {
	got := ToModelValuation(RawValuation{TsCode: "600000.SH", TurnoverRate: 1.23, PETtm: 6.7, PB: 0.82, CircMv: 30907817.94})
	if got.TurnoverRate != 1.23 || got.PETtm != 6.7 || got.PB != 0.82 || got.TsCode != "600000.SH" {
		t.Errorf("字段透传有误: %+v", got)
	}
	// 浦发银行实值 3090 亿：确认万元口径下与 thresholds（50 亿 = 500000 万元）比较是合理的。
	var v model.Valuation = got
	if v.CircMvW < 500000 {
		t.Errorf("浦发银行 %.0f 万元应高于 50 亿门槛 500000 万元", v.CircMvW)
	}
}
