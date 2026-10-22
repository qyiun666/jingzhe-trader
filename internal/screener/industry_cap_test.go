package screener

import (
	"testing"

	"jingzhe-trader/internal/model"
)

// TestCappedRank 行业上限的纯选取逻辑：按分数序取，每行业最多 limit 只；
// 跳过超限项后继续向下取，名次保留原始排名（1-based，允许空洞）；未知行业放行。
func TestCappedRank(t *testing.T) {
	scored := []Scored{{Code: "a"}, {Code: "b"}, {Code: "c"}, {Code: "d"}, {Code: "e"}}
	ind := map[string]string{"a": "电子", "b": "电子", "c": "电子", "d": "电子", "e": "银行"}

	picked, ranks, drops := cappedRank(scored, ind, 3, 5)
	if len(picked) != 4 || drops != 1 {
		t.Fatalf("应取 4 只、跳过 1 只（超限），实际 picked=%d drops=%d", len(picked), drops)
	}
	gotCodes := []string{picked[0].Code, picked[1].Code, picked[2].Code, picked[3].Code}
	wantCodes := []string{"a", "b", "c", "e"}
	for i := range wantCodes {
		if gotCodes[i] != wantCodes[i] {
			t.Fatalf("选取顺序=%v，期望 %v", gotCodes, wantCodes)
		}
	}
	wantRanks := []int{1, 2, 3, 5}
	for i := range wantRanks {
		if ranks[i] != wantRanks[i] {
			t.Errorf("第 %d 只排名=%d，期望 %d", i, ranks[i], wantRanks[i])
		}
	}

	// limit=0 关闭上限：全部按序取。
	picked, _, drops = cappedRank(scored, ind, 0, 5)
	if len(picked) != 5 || drops != 0 {
		t.Errorf("关闭上限应取满 5 只、0 跳过，实际 %d/%d", len(picked), drops)
	}

	// 未知行业不参与计数，直接放行：a/b 电子（满 2），c 超限跳过，d/e 未知放行。
	ind2 := map[string]string{"a": "电子", "b": "电子", "c": "电子", "d": "", "e": ""}
	picked, _, drops = cappedRank(scored, ind2, 2, 5)
	if len(picked) != 4 || drops != 1 {
		t.Errorf("未知行业应放行，实际 picked=%d drops=%d", len(picked), drops)
	}
	if picked[2].Code != "d" {
		t.Errorf("第 3 只应为未知行业 d，实际 %q", picked[2].Code)
	}
}

// TestPickTopNCapsIndustry 漏斗侧 TopN 截断：同行业超过上限时，名额让给后续行业，
// 且候选必须是保留带成员（买入第二天不会被规则 4 当成"跌出榜"）。
func TestPickTopNCapsIndustry(t *testing.T) {
	codes := []string{"e1", "e2", "e3", "e4", "b1", "h1"}
	industries := []string{"电子", "电子", "电子", "电子", "银行", "医药"}
	const bars = 20
	closes := make(map[string][]float64, len(codes))
	raws := make(map[string][]float64, len(codes))
	vols := make(map[string][]float64, len(codes))
	pool := make([]model.StockBasic, 0, len(codes))
	for i, code := range codes {
		growth := float64(len(codes)-i) / 100 // 排名顺序 e1>e2>...>h1
		series := make([]float64, bars)
		raw := make([]float64, bars)
		vol := make([]float64, bars)
		for j := 0; j < bars; j++ {
			series[j] = 10 * (1 + growth*float64(j)/float64(bars-1))
			raw[j] = series[j] * 100
			vol[j] = 1000
		}
		closes[code], raws[code], vols[code] = series, raw, vol
		pool = append(pool, model.StockBasic{
			TsCode: code, Name: code, Industry: industries[i], ListStatus: "L",
			PETtm: 10, PB: 1, TurnoverRate: 2, CircMvW: 1000000,
		})
	}
	in := &inputs{closes: closes, raws: raws, vols: vols}
	s := &Screener{
		cfg: FilterConfig{TopN: 5, MaxPerIndustry: 3},
		w:   WeightsByMode(ModeMomentum),
	}
	top, watch := s.pickTopN(pool, in, nil, &tracer{rep: &Report{}})
	if len(top) != 5 {
		t.Fatalf("TopN 应取满 5 只，实际 %d: %+v", len(top), top)
	}
	elec := 0
	inWatch := make(map[string]bool, len(watch))
	for _, c := range watch {
		inWatch[c.TsCode] = true
	}
	for _, c := range top {
		if c.Industry == "电子" {
			elec++
		}
		if !inWatch[c.TsCode] {
			t.Errorf("候选 %s 不在保留带内", c.TsCode)
		}
	}
	if elec != 3 {
		t.Fatalf("电子行业应被截到 3 只，实际 %d: %+v", elec, top)
	}
	// 第 4 只电子（e4）必须被行业上限挤掉，名额让给银行/医药。
	for _, c := range top {
		if c.TsCode == "e4" {
			t.Errorf("e4 是第 4 只电子，应被行业上限跳过: %+v", top)
		}
	}
}
