package screener

import (
	"fmt"
	"math"
	"sort"

	"jingzhe-trader/internal/model"
)

// FactorWeights 四因子权重（各档位可覆盖；权重**可为负**，负号表示该因子反向使用）。
//
// 评分口径：Composite = Σ wᵢ × scoreᵢ，scoreᵢ 是"该因子原始方向的截面百分位"
// （动量=涨幅高、低波=波动低、流动性=换手高、价值=越便宜越高）。
// 因此 wᵢ 为正表示"越高越买"，为负表示"反向使用"。
type FactorWeights struct {
	Momentum  float64
	Value     float64
	LowVol    float64
	Liquidity float64
}

// DefaultWeights 均衡权重（原始方向，合计 1.0）。
//
// ⚠ 这是长期沿用的手拍权重，未经 IC 验证。2023-09~2026-09 的 IC 实测显示它的
// 综合分是**反向**的（2026-09-15：20 日 RankIC −0.0896，ICIR −0.648，样本 689 个截面，
// 前后两段独立样本同为负）——保留它只为向后兼容与 A/B 对照，
// 有证据的替代是 ReversalWeights（见 screen.factor_mode 配置）。
func DefaultWeights() FactorWeights {
	return FactorWeights{Momentum: 0.30, Value: 0.25, LowVol: 0.20, Liquidity: 0.25}
}

// ReversalWeights 按 2023-09~2026-09 IC 实测得出的方向权重（Σ|w| = 1）。
//
// 实测结论（20 日 RankIC，可投池，n=689 个截面；前后两段独立复核）：
//
//	动量 −0.081（前段 −0.073 / 后段 −0.087）→ 反向
//	流动性(换手) −0.076（−0.072 / −0.078）→ 反向
//	低波 −0.076（−0.103 / −0.055）→ 反向
//	价值 +0.026（+0.041 / +0.015）→ 方向为正但弱，给较小正权重
//
// 三条负 IC 与 A 股"月频反转、高换手/高波动透支"的文献结论一致。
// 幅度按 |IC| 归一到合计 1（价值因未达 |IC|≥0.03 门槛，按最小可辨权重 0.1 给）。
//
// 这不是"最优参数"：它只把已被数据否定的三个方向掉头，未做参数寻优
// （本仓已有先例：MA40 全样本最优因属样本内挑参而弃用）。方向由 IC 决定，
// 幅度保持朴素，避免过拟合。
func ReversalWeights() FactorWeights {
	return FactorWeights{Momentum: -0.30, Value: 0.10, LowVol: -0.30, Liquidity: -0.30}
}

// WeightsByMode 按配置的模式取权重。
//
//	"momentum"（默认，向后兼容）：原始方向
//	"reversal"（IC 验证）：反向使用动量/低波/流动性
//
// 未知取值回落到默认：装配期 validateEnums 会拒绝非法值，但 CLI 的 db/research/config
// 等入口不走装配（见 research_cmd 的调用点），回落是这些入口的兜底而非静默容错。
func WeightsByMode(mode string) FactorWeights {
	if mode == ModeReversal {
		return ReversalWeights()
	}
	return DefaultWeights()
}

// ValidFactorMode 模式取值是否合法（供 config set 与装配期共用同一份判据）。
func ValidFactorMode(mode string) bool {
	return mode == ModeMomentum || mode == ModeReversal
}

// 因子方向模式取值（config screen.factor_mode）。
const (
	ModeMomentum = "momentum"
	ModeReversal = "reversal"
)

// neutralScore 缺失数据的因子中性分。
const neutralScore = 50.0

// RawMetrics 单只股票的原始因子输入（截面百分位转换前）。
type RawMetrics struct {
	Momentum float64 // 区间收益率（小数，如 0.12）
	Value    float64 // 估值原始值：-1 表示越便宜越好（由调用方直接给"越低越好"的合成值）
	LowVol   float64 // 区间日收益率标准差
	Turnover float64 // 换手率（%）
	OK       bool    // 行情序列是否足够（不足以计算动量/低波）
}

// ComputeRaw 计算单只股票的原始因子值。closes 为升序前复权收盘。
// bars 不足 minBars 时 OK=false（动量/低波取中性分）。
func ComputeRaw(closes []float64, turnover float64, minBars int) RawMetrics {
	rm := RawMetrics{Turnover: turnover}
	if len(closes) < minBars {
		return rm
	}
	rm.OK = true
	first, last := closes[0], closes[len(closes)-1]
	if first > 0 {
		rm.Momentum = last/first - 1
	}
	rets := dailyReturns(closes)
	rm.LowVol = stdDev(rets)
	// 价值：用 PE_TTM 与 PB 的倒数合成（越低越好的原始值交由截面百分位反转），
	// 由调用方在 scorePool 中直接填 rm.Value = -(合成便宜的度量)。
	return rm
}

// dailyReturns 日收益率序列（长度 = len-1）。
func dailyReturns(closes []float64) []float64 {
	if len(closes) < 2 {
		return nil
	}
	rets := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		if closes[i-1] > 0 {
			rets = append(rets, closes[i]/closes[i-1]-1)
		}
	}
	return rets
}

// stdDev 总体标准差。
func stdDev(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	mean := 0.0
	for _, x := range v {
		mean += x
	}
	mean /= float64(len(v))
	ss := 0.0
	for _, x := range v {
		ss += (x - mean) * (x - mean)
	}
	return math.Sqrt(ss / float64(len(v)))
}

// PercentileRank 截面百分位（0~100）：值越大百分位越高。
// 输入含 NaN 的位置返回 neutralScore；并列值取平均秩。
func PercentileRank(vals []float64) []float64 {
	n := len(vals)
	out := make([]float64, n)
	if n == 0 {
		return out
	}
	// 仅对有效值排序
	idx := make([]int, 0, n)
	for i, v := range vals {
		if !math.IsNaN(v) {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		for i := range out {
			out[i] = neutralScore
		}
		return out
	}
	sort.Slice(idx, func(a, b int) bool { return vals[idx[a]] < vals[idx[b]] })
	// 并列值取平均秩
	i := 0
	for i < len(idx) {
		j := i
		for j+1 < len(idx) && vals[idx[j+1]] == vals[idx[i]] {
			j++
		}
		avgRank := float64(i+j) / 2 // 0-based 平均秩
		pct := avgRank / float64(len(idx)-1) * 100
		if len(idx) == 1 {
			pct = neutralScore
		}
		for k := i; k <= j; k++ {
			out[idx[k]] = pct
		}
		i = j + 1
	}
	for i := range out {
		if math.IsNaN(vals[i]) {
			out[i] = neutralScore
		}
	}
	return out
}

// Scored 单只股票的五因子得分与综合分（内部中间结构）。
type Scored struct {
	Code    string
	Factors model.FactorScore
	Score   float64
}

// Composite 加权综合分（0~100）。
func Composite(fs model.FactorScore, w FactorWeights) float64 {
	return fs.Momentum*w.Momentum + fs.Value*w.Value +
		fs.LowVol*w.LowVol + fs.Liquidity*w.Liquidity
}

// buildFactorScores 将原始指标截面转换为因子百分位分（0~100）。
// pePB 传 {PE_TTM, PB}，价值分由二者倒数合成（越大越便宜）。
func buildFactorScores(codes []string, raw map[string]RawMetrics, pePB map[string][2]float64) map[string]model.FactorScore {
	return BuildFactorScores(codes, raw, pePB)
}

// BuildFactorScores 是 buildFactorScores 的导出入口，供回测/IC 复用。
//
// 必须只有这一份实现：IC 度量若另写一套因子算法，测出来的就不是选股器真正用的东西，
// "因子有没有选股能力"这个结论会建立在一段与生产无关的代码上。
func BuildFactorScores(codes []string, raw map[string]RawMetrics, pePB map[string][2]float64) map[string]model.FactorScore {
	n := len(codes)
	mom := make([]float64, n)
	val := make([]float64, n)
	low := make([]float64, n)
	liq := make([]float64, n)
	for i, c := range codes {
		rm := raw[c]
		mom[i] = rm.Momentum
		if !rm.OK {
			mom[i] = math.NaN()
			low[i] = math.NaN()
		} else {
			low[i] = rm.LowVol
		}
		// 价值合成：PE 与 PB 各自倒数平均（越大越便宜），缺失用另一项
		if pp, ok := pePB[c]; ok {
			invPE, invPB := 0.0, 0.0
			if pp[0] > 0 {
				invPE = 1 / pp[0]
			}
			if pp[1] > 0 {
				invPB = 1 / pp[1]
			}
			switch {
			case invPE > 0 && invPB > 0:
				val[i] = (invPE + invPB) / 2
			case invPE > 0:
				val[i] = invPE
			default:
				val[i] = invPB
			}
		} else {
			val[i] = math.NaN()
		}
		liq[i] = rm.Turnover
		if rm.Turnover <= 0 {
			liq[i] = math.NaN()
		}
	}
	pMom := PercentileRank(mom)
	pVal := PercentileRank(val) // 越大越便宜 → 直接为"价值分"
	pLow := PercentileRank(low)
	pLiq := PercentileRank(liq)

	out := make(map[string]model.FactorScore, n)
	for i, c := range codes {
		out[c] = model.FactorScore{
			Momentum:  pMom[i],
			Value:     pVal[i],
			LowVol:    pLow[i],
			Liquidity: pLiq[i],
		}
	}
	return out
}

// BuildReason 生成可解释理由文本（指令单 reason 的唯一来源，必须人可读）。
func BuildReason(fs model.FactorScore, w FactorWeights, score float64, s model.StockBasic) string {
	return fmt.Sprintf("综合%.1f分；%s；PE_TTM %.1f/PB %.1f/换手 %.2f%%/流通市值 %.0f万",
		score, topFactorNames(fs, w), s.PETtm, s.PB, s.TurnoverRate, s.CircMvW)
}

// topFactorNames 取得分贡献最高的两个因子名（"动量85/低波72"样式）。
//
// 按**加权贡献** wᵢ·scoreᵢ 排序而不是按因子分绝对值：反向模式下动量/低波/流动性
// 的权重为负，分数越高越是压低综合分，此时把它们的原始分列为"亮点"会给出相反的理由。
// 展示的数值仍是该因子的截面分位（人读的是"这个因子多少分"），只是排序口径按贡献。
func topFactorNames(fs model.FactorScore, w FactorWeights) string {
	type kv struct {
		name string
		v    float64 // 因子截面分（展示用）
		cont float64 // 加权贡献（排序用）
	}
	all := []kv{
		{"动量", fs.Momentum, w.Momentum * fs.Momentum},
		{"价值", fs.Value, w.Value * fs.Value},
		{"低波", fs.LowVol, w.LowVol * fs.LowVol},
		{"流动性", fs.Liquidity, w.Liquidity * fs.Liquidity},
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].cont > all[j].cont })
	return fmt.Sprintf("%s%.0f/%s%.0f", all[0].name, all[0].v, all[1].name, all[1].v)
}
