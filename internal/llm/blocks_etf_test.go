package llm

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/signal"
)

// etfRequest 一只走 ETF 漏斗的候选（无 PE/PB/换手/流通市值/排名信息，全为 Go 零值）。
func etfRequest() signal.BuyRequest {
	return signal.BuyRequest{
		TradeDate: "20261009",
		Candidate: model.Candidate{
			Rank: 1, Kind: model.KindETF, TsCode: "588000.SH", Name: "科创50ETF",
			Industry: "科创50", Close: model.Fen(91),
			Reason: "ETF 白名单直通（窗口 20 日，不排名）",
		},
		Bars:    signal.BarSeries{Closes: bars(20, 0.91), Vols: bars(20, 5e6), Raws: bars(20, 91)},
		Rules:   signal.RuleEvidence{MA5: 0.91, MA20: 0.90, TrendUp: true, VolRatio: 1.1, RetPct: 1.2, OffHighPct: -2.0, VolPct: 2.3, MaxDropPct: -3.1, AvgAmtYuan: 4.55e8},
		RulesOK: true,
		Budget: signal.BuyBudget{CashFen: model.Fen(1123379), SlotFen: model.Fen(118268),
			LotCostFen: model.Fen(9100), Positions: 1, MaxPos: 2},
	}
}

// stockRequest 同批次的个股候选（对照用：它的段落必须照旧带估值栏）。
func stockRequest() signal.BuyRequest {
	r := etfRequest()
	r.Candidate = model.Candidate{
		Rank: 2, Kind: model.KindStock, TsCode: "600908.SH", Name: "无锡银行", Industry: "银行",
		Close: model.Fen(593), CircMvW: 82.5, PETtm: 5.1, PB: 0.62, TurnoverRate: 0.85,
		Score: 61.5, PoolSize: 5326, Reason: "低估值 + 流动性达标",
	}
	return r
}

func bars(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestEtfHeaderNoZeroValuation ETF 段落绝不能出现"PE(TTM) 0.0 / PB 0.00"这类
// 由零值渲染出来的假数据：模型会把"估值为 0"读成极度便宜，而事实是这项数据不存在。
// （字段名以"我们没有这项"的形式出现是允许的，被禁止的是给它编一个数值。）
func TestEtfHeaderNoZeroValuation(t *testing.T) {
	it := etfRequest()
	got := headerBlock(it)
	for _, bad := range []string{"PE(TTM)", "PB 0", "换手率 0", "流通市值 0", "综合分", "百分位"} {
		if strings.Contains(got, bad) {
			t.Errorf("ETF 段不应给个股字段编出数值 %q，实际:\n%s", bad, got)
		}
	}
	for _, want := range []string{"场内 ETF", "T+1", "免印花税", "100 份", "科创50", "无 PE / PB"} {
		if !strings.Contains(got, want) {
			t.Errorf("ETF 段缺少 %q，实际:\n%s", want, got)
		}
	}
	// 价格精度：0.91 元不能被写成 0.9 之外的两位小数形式掩盖第三位（这里按三位展示）。
	if !strings.Contains(got, "0.910 元") {
		t.Errorf("ETF 收盘应按三位小数展示（厘），实际:\n%s", got)
	}
}

// TestEtfDimBlocksHonest 四个证据维度对 ETF 的如实程度：
// 估值维度必须引导模型答 unknown，板块维度不得把未计算的两栏渲染成 0。
func TestEtfDimBlocksHonest(t *testing.T) {
	it := etfRequest()
	if v := valueBlock(it); !strings.Contains(v, "unknown=true") || strings.Contains(v, "PE(TTM)") {
		t.Errorf("估值维度应要求承认无数据且不编出 PE 数值，实际:\n%s", v)
	}
	s := sectorBlock(it)
	for _, want := range []string{"跟踪方向", "不排名", "日均成交额"} {
		if !strings.Contains(s, want) {
			t.Errorf("板块段缺 %q，实际:\n%s", want, s)
		}
	}
	if strings.Contains(s, "板块20日加权动量") || strings.Contains(s, "流通市值 0") {
		t.Errorf("板块段不得出现 ETF 没有的两栏，实际:\n%s", s)
	}
	n := newsBlock(it)
	for _, want := range []string{"跟踪指数成分调整", "份额折算", "清盘"} {
		if !strings.Contains(n, want) {
			t.Errorf("消息面检索任务应换成 ETF 口径，缺 %q，实际:\n%s", want, n)
		}
	}
	// 检索任务清单里不得混进个股风险项（"不承担…质押爆仓"这种否定说明是允许且必要的）。
	for _, bad := range []string{"查 \"科创50ETF 588000\"：立案调查", "窗口内近似跌停次数 0"} {
		if strings.Contains(n, bad) {
			t.Errorf("消息面不得用个股口径否决 ETF（%q），实际:\n%s", bad, n)
		}
	}
	// 技术段是个股与 ETF 共用的真实计算量（MA/量比/波动），不该被特殊化。
	if tk := techBlock(it); !strings.Contains(tk, "MA5") {
		t.Errorf("技术段应保持原样，实际:\n%s", tk)
	}
}

// TestStockBlocksUnchanged 对照：个股那套段落一个字都没变（本次改动只新增分支）。
func TestStockBlocksUnchanged(t *testing.T) {
	it := stockRequest()
	got := headerBlock(it)
	for _, want := range []string{"PE(TTM) 5.1", "PB 0.62", "综合分 61.5", "银行"} {
		if !strings.Contains(got, want) {
			t.Errorf("个股段应保持 %q，实际:\n%s", want, got)
		}
	}
	if strings.Contains(got, "免印花税") {
		t.Errorf("个股段不得混入 ETF 说明，实际:\n%s", got)
	}
	if v := valueBlock(it); !strings.Contains(v, "PE(TTM) 5.1") {
		t.Errorf("个股估值段应保持原样，实际:\n%s", v)
	}
	if n := newsBlock(it); !strings.Contains(n, "风险公告") {
		t.Errorf("个股消息面检索任务应保持原样，实际:\n%s", n)
	}
}

// TestEtfHoldingBlock 已持有 ETF 时名称单位与价格精度都要对：
// 说"1000 股"会让用户以为可以按股申报，而 ETF 最小申报是 100 份。
func TestEtfHoldingBlock(t *testing.T) {
	it := etfRequest()
	h := model.Position{TsCode: "588000.SH", TotalQty: model.Qty(1100), CostPrice: 90, HighPrice: 95}
	it.Holding = &h
	got := holdingBlock(it)
	if !strings.Contains(got, "1100 份") || strings.Contains(got, "股") {
		t.Errorf("ETF 持仓段应按份表述，实际:\n%s", got)
	}
	if !strings.Contains(got, "0.900 元") || !strings.Contains(got, "0.910 元") {
		t.Errorf("ETF 价格应三位小数（成本 0.90/现价 0.91），实际:\n%s", got)
	}
	if strings.Contains(got, "加仓") == false {
		t.Errorf("已持有必须写明这是加仓判断，实际:\n%s", got)
	}
}

// TestEtfDecisionUserMergesBothKinds 混批：一次决策 prompt 同时含个股与 ETF 段落，
// ts_code 清单两只都在（风控与回传契约按代码收口，不按品种分叉）。
func TestEtfDecisionUserMergesBothKinds(t *testing.T) {
	etf, stock := etfRequest(), stockRequest()
	items := []signal.BuyRequest{etf, stock}
	concl := map[string][]string{etf.Candidate.TsCode: {"技术：中性"}, stock.Candidate.TsCode: {"估值：正面"}}
	got := decisionUser(items, concl)
	for _, want := range []string{"588000.SH", "600908.SH", "科创50ETF", "PE(TTM) 5.1", "免印花税", "可用现金"} {
		if !strings.Contains(got, want) {
			t.Errorf("混批 prompt 缺 %q", want)
		}
	}
	if n := strings.Count(got, "——— 第"); n != 2 {
		t.Errorf("混批应有两段，实际 %d 段", n)
	}
}

// TestEtfEvidenceUserPerDim 证据 prompt 混批：每段只出现该维度的块，ETF 段不混个股字段。
func TestEtfEvidenceUserPerDim(t *testing.T) {
	items := []signal.BuyRequest{etfRequest(), stockRequest()}
	for _, p := range []struct{ key, title string }{{KeyTech, "技术形态"}, {KeyValue, "估值"},
		{KeyNews, "消息面"}, {KeySector, "板块与冲击成本"}} {
		got := evidenceUser(PromptSpec{Key: p.key, Title: p.title}, items)
		if strings.Contains(got, "0.00 亿") || strings.Contains(got, "PE(TTM) 0.0") {
			t.Errorf("%s 维度把 ETF 的空字段渲染成了零值，实际:\n%s", p.key, got)
		}
	}
}
