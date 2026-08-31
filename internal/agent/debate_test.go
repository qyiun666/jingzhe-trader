package agent

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestDebateAdjustedQty position_pct 的口径必须是"拟买入额占总资产比例"。
// 曾有实现把它当作对 TargetQty 的缩放系数: LLM 答常规仓 0.35 会被砍到计划量的 35%,
// 答重仓 0.6 仍然是砍仓 —— 没有任何取值能维持原计划。
func TestDebateAdjustedQty(t *testing.T) {
	const (
		totalAsset = 12000.0
		price      = 10.0
		maxPosPct  = 0.40
		// 计划量必须与策略同一公式得出: 12000×0.40/10 = 480 股, 向下取整到整手 = 400
		planned = 400
	)
	bar := &model.Bar{Close: price}

	cases := []struct {
		desc      string
		suggested float64
		want      int
	}{
		{"建议等于风控上限 → 维持计划量", 0.40, planned},
		{"建议超过上限 → 按上限裁, 仍等于计划量", 0.60, planned},
		// 0.20 档 = 240 股, 向下取整到整手为 200: 换算必须与策略一样遵守整手约束
		{"建议半仓 → 缩量并按整手取整", 0.20, 200},
		{"建议未给出(0) → 不改计划", 0, planned},
		{"建议为负 → 不改计划", -0.5, planned},
	}
	for _, c := range cases {
		if got := debateAdjustedQty(planned, totalAsset, bar, c.suggested, maxPosPct); got != c.want {
			t.Errorf("%s: 得到 %d, 期望 %d", c.desc, got, c.want)
		}
	}

	// 换算失败不得反向改变仓位
	if got := debateAdjustedQty(planned, totalAsset, nil, 0.2, maxPosPct); got != planned {
		t.Errorf("缺价格时应保持计划量, 得到 %d", got)
	}
	if got := debateAdjustedQty(planned, 0, bar, 0.2, maxPosPct); got != planned {
		t.Errorf("总资产缺失时应保持计划量, 得到 %d", got)
	}
}

// TestDebateAdjustedQtyNeverEnlarges 辩论只能缩量: 放大必须交给统一风控, 否则这里会绕过 max_position_pct
func TestDebateAdjustedQtyNeverEnlarges(t *testing.T) {
	small := 100 // 策略因资金/门槛只计划买 100 股
	got := debateAdjustedQty(small, 12000, &model.Bar{Close: 10}, 0.40, 0.40)
	if got != small {
		t.Errorf("辩论不应放大仓位: 得到 %d, 期望保持 %d", got, small)
	}
}

// TestIsMissingData 降级报告必须能与模型真实给出的低置信度区分开
func TestIsMissingData(t *testing.T) {
	degraded := &AnalysisReport{Agent: "news", KeyPoints: []string{reportMissingData}}
	if !degraded.IsMissingData() {
		t.Error("降级报告应识别为数据缺失")
	}
	// 模型真的给出 confidence 0 时不该被误判为缺失
	real := &AnalysisReport{Agent: "news", Confidence: 0, KeyPoints: []string{"无相关新闻"}}
	if real.IsMissingData() {
		t.Error("有依据的报告不应被标记为数据缺失")
	}
	var nilReport *AnalysisReport
	if !nilReport.IsMissingData() {
		t.Error("nil 报告应视为缺失")
	}
}

// TestFallbackJudgeExcludesMissingReports 无依据报告以 0.0 计入均值会把结果系统性拉向 reject
func TestFallbackJudgeExcludesMissingReports(t *testing.T) {
	rm := NewRiskManagerAgent(nil, PositionLimits{MaxPositionPct: 0.4, MaxTotalPositionPct: 0.9, StopLossPct: 0.08})
	reports := []*AnalysisReport{
		{Agent: "technical", Sentiment: 0.6, Confidence: 0.7},
		{Agent: "fundamental", Sentiment: 0.5, Confidence: 0.6},
		{Agent: "news", KeyPoints: []string{reportMissingData}},
		{Agent: "market", KeyPoints: []string{reportMissingData}},
	}
	res := rm.fallbackJudge(&DebateContext{TsCode: "A.SH", TradeDate: "20260831"}, reports, nil, nil)

	// 有效均值 (0.6+0.5)/2 = 0.55 → blended=(0.55+0+0)/3 ≈ 0.183 > 0.15 → buy
	// 若把两条缺失报告按 0.0 计入, 均值只有 0.275, blended≈0.09 会落到 hold
	if res.Decision != "buy" {
		t.Errorf("缺失报告不应拉低均值, 决策应为 buy, 实际 %s (summary=%s)", res.Decision, res.Summary)
	}
	if res.PositionPct > 0.4 {
		t.Errorf("降级建议仓位不得超过风控上限 0.40, 实际 %.2f", res.PositionPct)
	}
}

// TestFallbackJudgeAllMissing 全部分析师无依据时不该给出买入
func TestFallbackJudgeAllMissing(t *testing.T) {
	rm := NewRiskManagerAgent(nil, PositionLimits{MaxPositionPct: 0.4, MaxTotalPositionPct: 0.9, StopLossPct: 0.08})
	reports := []*AnalysisReport{
		{Agent: "technical", KeyPoints: []string{reportMissingData}},
		{Agent: "fundamental", KeyPoints: []string{reportMissingData}},
	}
	res := rm.fallbackJudge(&DebateContext{TsCode: "A.SH"}, reports, nil, nil)
	if res.Decision != "hold" {
		t.Errorf("全无依据时应 hold, 实际 %s", res.Decision)
	}
}

// TestSystemPromptReflectsRiskLimits 提示词里的仓位/止损数字必须来自注入的风控配置
func TestSystemPromptReflectsRiskLimits(t *testing.T) {
	rm := NewRiskManagerAgent(nil, PositionLimits{MaxPositionPct: 0.4, MaxTotalPositionPct: 0.9, StopLossPct: 0.08})
	p := rm.systemPrompt()
	for _, want := range []string{"0~0.40", "总仓位上限 90%", "现价下方约 8%"} {
		if !strings.Contains(p, want) {
			t.Errorf("提示词缺少基于配置生成的 %q", want)
		}
	}
	// 旧的硬编码值不得复现
	for _, forbidden := range []string{"0.5-0.6", "不超过总资产60%"} {
		if strings.Contains(p, forbidden) {
			t.Errorf("提示词残留硬编码值 %q", forbidden)
		}
	}
}

// TestSanitizeStopPrice 买入决策的止损价必须可执行: 0 或高于现价的价格都要被换掉
func TestSanitizeStopPrice(t *testing.T) {
	rm := NewRiskManagerAgent(nil, PositionLimits{MaxPositionPct: 0.4, MaxTotalPositionPct: 0.9, StopLossPct: 0.08})

	// 模型回 0 (当成"不设止损") → 按现价下方 8% 兜底
	res := &DebateResult{TsCode: "A.SH", Decision: "buy", StopPrice: 0}
	rm.sanitizeStopPrice(res, 10.0)
	if res.StopPrice != 9.2 {
		t.Errorf("止损价应按止损线兜底为 9.20, 实际 %.2f", res.StopPrice)
	}

	// 模型回一个高于现价的价格 → 照它做会立刻触发卖出, 必须换掉
	res = &DebateResult{TsCode: "A.SH", Decision: "buy", StopPrice: 12.5}
	rm.sanitizeStopPrice(res, 10.0)
	if res.StopPrice != 9.2 {
		t.Errorf("高于现价的止损价不可用, 实际 %.2f", res.StopPrice)
	}

	// 合理价格原样保留
	res = &DebateResult{TsCode: "A.SH", Decision: "buy", StopPrice: 9.55}
	rm.sanitizeStopPrice(res, 10.0)
	if res.StopPrice != 9.55 {
		t.Errorf("合理止损价不应被改写, 实际 %.2f", res.StopPrice)
	}

	// 非买入决策不带止损价; 无现价时也不编造
	res = &DebateResult{TsCode: "A.SH", Decision: "reject", StopPrice: 9.55}
	rm.sanitizeStopPrice(res, 10.0)
	if res.StopPrice != 0 {
		t.Errorf("reject 决策不应保留止损价, 实际 %.2f", res.StopPrice)
	}
	res = &DebateResult{TsCode: "A.SH", Decision: "buy", StopPrice: 9.55}
	rm.sanitizeStopPrice(res, 0)
	if res.StopPrice != 0 {
		t.Errorf("无现价时不得凭空给止损价, 实际 %.2f", res.StopPrice)
	}
}

// TestNoDataReportExcludedFromMean 无输入维度 (无新闻/无基本面) 必须与"分析失败"一样不计入均值
func TestNoDataReportExcludedFromMean(t *testing.T) {
	r := noDataReport("news", "A.SH", "近7日无该股相关新闻")
	if !r.IsMissingData() {
		t.Fatal("无输入报告应识别为数据缺失")
	}
	if !strings.Contains(r.KeyPoints[0], "近7日无该股相关新闻") {
		t.Errorf("应保留具体缺失原因, 实际 %q", r.KeyPoints[0])
	}
}
