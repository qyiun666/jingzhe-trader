package signal

import (
	"context"
	"math"
	"testing"

	"jingzhe-trader/internal/model"
)

// rampSeries 造一段单调上升的收盘与固定成交量序列。
func rampSeries(n int, step, vol float64) BarSeries {
	bs := BarSeries{Closes: make([]float64, n), Vols: make([]float64, n), Raws: make([]float64, n)}
	for i := 0; i < n; i++ {
		bs.Closes[i] = 10 + float64(i)*step
		bs.Vols[i] = vol
		bs.Raws[i] = (10 + float64(i)*step) * 100 // 元 → 分
	}
	return bs
}

// TestEvaluateRulesTrendUp 上升趋势 + 末日放量：证据列应当如实描述形态，但不做任何"该不该买"的判断。
func TestEvaluateRulesTrendUp(t *testing.T) {
	bs := rampSeries(30, 0.2, 100)
	bs.Vols[29] = 150
	e, ok := EvaluateRules(bs)
	if !ok {
		t.Fatalf("30 根序列应能算出证据")
	}
	if !e.TrendUp {
		t.Errorf("MA5 应高于 MA20: %+v", e)
	}
	if e.VolRatio < 1.2 {
		t.Errorf("末日放量 1.5 倍，量比应 ≥1.2，实际 %.2f", e.VolRatio)
	}
	if e.RetPct <= 0 {
		t.Errorf("单调上升的区间涨幅应为正，实际 %.2f", e.RetPct)
	}
	if e.OffHighPct != 0 {
		t.Errorf("收于窗口最高点时距高点应为 0，实际 %.2f", e.OffHighPct)
	}
}

// TestEvaluateRulesInsufficientBars 数据不足 20 根时必须显式报"算不出"，
// 不能让调用方拿部分窗口当完整窗口去评审。
func TestEvaluateRulesInsufficientBars(t *testing.T) {
	bs := rampSeries(19, 0.2, 100)
	if _, ok := EvaluateRules(bs); ok {
		t.Errorf("19 根不应算作有效证据")
	}
}

// TestEvaluateRulesDropEvidence 崩盘形态要被记成价格异常证据（消息面 prompt 的唯一素材）。
func TestEvaluateRulesDropEvidence(t *testing.T) {
	bs := rampSeries(25, 0.1, 100)
	bs.Closes[20] = bs.Closes[19] * 0.90 // -10%：近似跌停
	bs.Closes[22] = bs.Closes[21] * 0.95 // -5%
	bs.Vols[22] = 400                    // 放量下跌
	last := len(bs.Closes) - 1
	bs.Closes[last] = bs.Closes[last-1] * 0.93 // 末位收跌，确保低于窗口高点
	e, ok := EvaluateRules(bs)
	if !ok {
		t.Fatalf("25 根应能算出证据")
	}
	if e.LimitDownDays != 1 {
		t.Errorf("近似跌停天数=%d, 期望 1", e.LimitDownDays)
	}
	if e.MaxDropPct > -9.9 {
		t.Errorf("单日最大跌幅应逼近 -10%%，实际 %.2f", e.MaxDropPct)
	}
	if e.VolDownDays == 0 {
		t.Errorf("放量下跌天数应为正，实际 %d", e.VolDownDays)
	}
	if e.OffHighPct >= 0 {
		t.Errorf("末收盘低于窗口高点，回撤应为负，实际 %.2f", e.OffHighPct)
	}
}

// TestEvaluateRulesAvgAmount 日均成交额：未复权收盘（分）× 成交量（手）= 元
// （10 元/股 × 100 手 = 10 万元；分的 1/100 与一手的 100 股恰好抵消）。
func TestEvaluateRulesAvgAmount(t *testing.T) {
	bs := rampSeries(20, 0, 100)
	for i := range bs.Raws {
		bs.Raws[i] = 1000 // 10 元
	}
	bs.Vols[1] = 300
	e, _ := EvaluateRules(bs)
	// (19×1000×100 + 1×1000×300) / 20 = 110000 元
	if math.Abs(e.AvgAmtYuan-110000) > 1 {
		t.Errorf("日均成交额=%.0f, 期望 110000 元", e.AvgAmtYuan)
	}
}

// TestClampWeight 模型给出的比例越界一律收口，不做解释性放大。
func TestClampWeight(t *testing.T) {
	cases := []struct{ in, want float64 }{{-1, 0}, {0, 0}, {0.3, 0.3}, {1, 1}, {3, 1}, {math.NaN(), 0}}
	for _, tc := range cases {
		if got := clampWeight(tc.in); got != tc.want {
			t.Errorf("clampWeight(%v)=%v, 期望 %v", tc.in, got, tc.want)
		}
	}
}

// TestNoDeciderNeverApproves llm.enabled=false 时的显式语义：没有决策者 = 整批不买，
// 绝不回落成"规则说了算"。
func TestNoDeciderNeverApproves(t *testing.T) {
	req := BatchRequest{TradeDate: "20260903", Items: []BuyRequest{
		{Candidate: model.Candidate{TsCode: "sh600001"}},
		{Candidate: model.Candidate{TsCode: "sz000002"}},
	}}
	got, err := NoDecider{}.DecideBatch(context.TODO(), req)
	if err != nil {
		t.Fatalf("NoDecider 不应报错: %v", err)
	}
	if (NoDecider{}).Enabled() {
		t.Error("NoDecider 必须报告未启用")
	}
	for _, it := range req.Items {
		d, ok := got[it.Candidate.TsCode]
		if !ok {
			t.Errorf("%s 没有结论行", it.Candidate.TsCode)
			continue
		}
		if d.Approve || d.Failed {
			t.Errorf("NoDecider 必须不批且不记失败（\"没有决策者\"是事实，不是\"不知道\"）: %+v", d)
		}
	}
}
