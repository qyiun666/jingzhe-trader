package screener

import (
	"fmt"
	"math"
	"sort"

	"jingzhe-trader/internal/model"
)

// FactorWeights 四因子权重（各档位可覆盖；均衡档偏动量，防御档偏低波与价值）。
type FactorWeights struct {
	Momentum  float64
	Value     float64
	LowVol    float64
	Liquidity float64
}

// DefaultWeights 均衡权重（合计 1.0）。
func DefaultWeights() FactorWeights {
	return FactorWeights{Momentum: 0.30, Value: 0.25, LowVol: 0.20, Liquidity: 0.25}
}

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
func BuildReason(fs model.FactorScore, score float64, s model.StockBasic) string {
	return fmt.Sprintf("综合%.1f分；%s；PE_TTM %.1f/PB %.1f/换手 %.2f%%/流通市值 %.0f万",
		score, topFactorNames(fs), s.PETtm, s.PB, s.TurnoverRate, s.CircMvW)
}

// topFactorNames 取得分最高的两个因子名（"动量85/低波72"样式）。
func topFactorNames(fs model.FactorScore) string {
	type kv struct {
		name string
		v    float64
	}
	all := []kv{
		{"动量", fs.Momentum}, {"价值", fs.Value},
		{"低波", fs.LowVol}, {"流动性", fs.Liquidity},
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].v > all[j].v })
	return fmt.Sprintf("%s%.0f/%s%.0f", all[0].name, all[0].v, all[1].name, all[1].v)
}
