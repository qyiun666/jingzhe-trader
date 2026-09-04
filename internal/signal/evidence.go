package signal

import (
	"math"
)

// RuleEvidence 规则算出的证据列。
//
// 用户拍板：买卖决策交给 LLM，规则信号（原 ma_cross 等）降级为喂进 prompt 的证据，
// 不再自己产指令 —— 本结构因此没有任何"是否通过"字段，只有数值。
// 其中后五项（MaxDropPct / LimitDownDays / VolDownDays / OffHighPct / VolPct）是
// 消息面 prompt 的唯一素材：接口拿不到新闻，只能用价格异常代替，且必须让模型知道这一点。
type RuleEvidence struct {
	TrendUp       bool    // MA5 > MA20
	MA5           float64 // 前复权收盘（元）
	MA20          float64
	VolRatio      float64 // 当日量 / 前 5 日均量
	RetPct        float64 // 窗口区间涨幅（%）
	OffHighPct    float64 // 距窗口最高收盘（%，≤0）
	VolPct        float64 // 日收益率标准差（%）
	MaxDropPct    float64 // 窗口内单日最大跌幅（%，≤0）
	LimitDownDays int     // 近似跌停天数（单日 ≤ -9.8%）
	VolDownDays   int     // 放量下跌天数（量 > 1.5× 均量且收跌）
	AvgAmtYuan    float64 // 窗口日均成交额（元）
}

// minEvidenceBars 证据计算所需的最少根数（与 MA20 同口径）。
const minEvidenceBars = 20

// nearLimitDownPct 近似跌停阈值：主板 10%，留 0.2% 容差；创业/科创 20% 无法由此判定（宁可漏判不误判）。
const nearLimitDownPct = -9.8

// EvaluateRules 由行情窗口计算证据列。数据不足 minEvidenceBars 根时 ok=false，
// 调用方必须把"证据不全"显式告诉 LLM，而不是拿部分窗口当完整窗口评审。
func EvaluateRules(bs BarSeries) (e RuleEvidence, ok bool) {
	if len(bs.Closes) < minEvidenceBars || len(bs.Vols) < minEvidenceBars {
		return e, false
	}
	e.MA5, e.MA20 = MA5MA20(bs.Closes)
	e.TrendUp = e.MA5 > e.MA20
	e.VolRatio = VolumeRatio(bs.Vols)

	first, last := bs.Closes[0], bs.Closes[len(bs.Closes)-1]
	e.RetPct = pctChange(last, first)
	var high float64
	var rets []float64
	var prev float64
	var amtSum float64
	avgVol := mean(bs.Vols[:len(bs.Vols)-1]) // 均量不含当日，避免自比
	for i, c := range bs.Closes {
		if c > high {
			high = c
		}
		if i > 0 {
			rets = append(rets, pctChange(c, prev))
		}
		prev = c
		if i < len(bs.Raws) {
			amtSum += bs.Raws[i] * bs.Vols[i] // 分 × 手 = 元（100 与 1/100 抵消）
		}
	}
	e.OffHighPct = pctChange(last, high)
	e.VolPct = stdevPct(rets)
	e.AvgAmtYuan = amtSum / float64(len(bs.Closes))
	for _, r := range rets {
		if r < e.MaxDropPct {
			e.MaxDropPct = r
		}
	}
	for i, r := range rets {
		if r <= nearLimitDownPct {
			e.LimitDownDays++
		}
		// rets[i] 对应 bs.Closes[i+1] / bs.Vols[i+1]：放量下跌 = 量 > 1.5× 均量且收跌
		if r < 0 && avgVol > 0 && bs.Vols[i+1] > 1.5*avgVol {
			e.VolDownDays++
		}
	}
	return e, true
}

// pctChange (now-prev)/prev×100；prev ≤ 0 返回 0（停牌/异常 0 价不参与）。
func pctChange(now, prev float64) float64 {
	if prev <= 0 {
		return 0
	}
	return (now - prev) / prev * 100
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// stdevPct 样本标准差（对已经是百分数的收益率）。
func stdevPct(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var sq float64
	for _, x := range xs {
		sq += (x - m) * (x - m)
	}
	return math.Sqrt(sq / float64(len(xs)-1))
}
