package screener

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
)

// TestCapitalPriceBand 资金反推价格带: 上限=一手买得起的价, 下限=一手凑不满最小单笔金额的价
func TestCapitalPriceBand(t *testing.T) {
	// 总资产1万 / 单票上限40% = 4000, 可用现金5000 → 取小 4000 → 上限 40 元
	// 最小单笔 3000 → 下限 30 元
	lo, hi := Capital{TotalAsset: 10000, Cash: 5000, MaxPositionPct: 0.4, MinTradeAmount: 3000}.PriceBand()
	if lo != 30 || hi != 40 {
		t.Fatalf("期望 [30,40], 实际 [%v,%v]", lo, hi)
	}

	// 现金才是约束: 已重仓后仅剩 1000 现金 → 上限 10 元
	_, hi = Capital{TotalAsset: 10000, Cash: 1000, MaxPositionPct: 0.4, MinTradeAmount: 3000}.PriceBand()
	if hi != 10 {
		t.Fatalf("现金更紧时应按现金反推, 期望上限10, 实际 %v", hi)
	}

	// 零值资金视图不施加任何约束 (退回配置里的静态区间)
	if lo, hi = (Capital{}).PriceBand(); lo != 0 || hi != 0 {
		t.Fatalf("零值应不限价, 实际 [%v,%v]", lo, hi)
	}
}

// TestPriceBandOfIntersectsConfig 资金价格带与配置区间取交集, 两侧都取更严的一条
func TestPriceBandOfIntersectsConfig(t *testing.T) {
	cfg := config.ScreenerConfig{MinPrice: 2, MaxPrice: 50}
	money := Capital{TotalAsset: 10000, Cash: 5000, MaxPositionPct: 0.4, MinTradeAmount: 3000}
	if lo, hi := priceBandOf(cfg, money); lo != 30 || hi != 40 {
		t.Fatalf("期望 [30,40], 实际 [%v,%v]", lo, hi)
	}
	// 无资金信息时完全退回配置
	if lo, hi := priceBandOf(cfg, Capital{}); lo != 2 || hi != 50 {
		t.Fatalf("期望退回配置 [2,50], 实际 [%v,%v]", lo, hi)
	}
	// 配置更严时以配置为准
	if lo, hi := priceBandOf(config.ScreenerConfig{MinPrice: 35, MaxPrice: 38}, money); lo != 35 || hi != 38 {
		t.Fatalf("期望以配置为准 [35,38], 实际 [%v,%v]", lo, hi)
	}
}

// TestFilterOneCapitalAndDirectionGates 资金与方向门槛逐条验证淘汰原因
func TestFilterOneCapitalAndDirectionGates(t *testing.T) {
	s := &Screener{cfg: config.ScreenerConfig{MaxPE: 50, MaxPB: 5, MinCircMV: 1}}
	base := model.DailyBasic{TsCode: "600000.SH", Close: 20, PE_TTM: 15, PB: 1, CircMV: 200000}
	// 由近及远 5 日收盘, MA5 = 19
	up := []float64{19.5, 19.2, 18.8, 18.5, 18.0}

	cases := []struct {
		name    string
		basic   model.DailyBasic
		recent  []float64
		band    [2]float64
		wantLog string
	}{
		{"通过", base, up, [2]float64{5, 40}, ""},
		{"高于资金上限", base, up, [2]float64{5, 15}, "高于资金可买价格带上限"},
		{"低于资金下限", base, up, [2]float64{25, 40}, "低于资金可买价格带下限"},
		{"趋势样本不足", base, []float64{19.5, 19.2}, [2]float64{0, 0}, "趋势数据不足"},
		{"收盘跌破MA5", model.DailyBasic{TsCode: base.TsCode, Close: 17, PE_TTM: 15, PB: 1, CircMV: 200000}, up, [2]float64{0, 0}, "收盘低于MA5"},
		{"动量为负", model.DailyBasic{TsCode: base.TsCode, Close: 19.4, PE_TTM: 15, PB: 1, CircMV: 200000},
			[]float64{19.0, 19.1, 19.2, 19.4, 19.6}, [2]float64{0, 0}, "5日动量为负"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := filterEnv{date: "20260831", recent: map[string][]float64{"600000.SH": tc.recent},
				minPrice: tc.band[0], maxPrice: tc.band[1]}
			_, rule := s.filterOne(tc.basic, env)
			if rule != tc.wantLog {
				t.Fatalf("淘汰原因应为 %q, 实际 %q", tc.wantLog, rule)
			}
		})
	}
}

// TestFilterOnePassesReasonDirection 通过门槛的候选, 入选理由必须写清方向依据
func TestFilterOnePassesReasonDirection(t *testing.T) {
	s := &Screener{}
	env := filterEnv{date: "20260831", recent: map[string][]float64{"600000.SH": {19.5, 19.2, 18.8, 18.5, 18.0}}}
	res, rule := s.filterOne(model.DailyBasic{TsCode: "600000.SH", Close: 20, CircMV: 200000}, env)
	if rule != "" {
		t.Fatalf("应通过筛选, 实际被淘汰: %s", rule)
	}
	if res.TradeDate != "20260831" {
		t.Errorf("候选应记录选股日期, 实际 %s", res.TradeDate)
	}
	if !strings.Contains(res.Reason, "方向向上") || !strings.Contains(res.Reason, "MA5") {
		t.Errorf("入选理由应包含方向依据, 实际 %q", res.Reason)
	}
}
