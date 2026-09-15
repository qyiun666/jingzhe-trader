package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/screener"
)

// FactorNames 参与 IC 度量的因子名（与 screener.FactorWeights 的字段一一对应）。
//
// composite = 当前生产权重（screen.factor_mode）的综合分；
// composite_rev = IC 验证的反向权重综合分。两者并排是为了直接对照方向修正的效果。
//
// 流动性（换手率）因子在这里也照测：它的方向是"追热 vs 反转"之争的核心，
// 光靠文献争论没有本系统的数据有说服力 —— 让 IC 说话。
var FactorNames = []string{"momentum", "value", "lowvol", "liquidity", "composite", "composite_rev"}

// validate 校验配置与生产因子口径一致。
//
// MinBars 只决定"窗口够不够算"，而因子值一律按 screener.BarWindow() 计算：
// 两者不一致时度量的就不是生产在用的那个因子，必须当场拒绝而不是静默算出一个
// 与选股器无关的 IC。
func (c Config) validate() error {
	if c.MinBars != 0 && c.MinBars != screener.BarWindow() {
		return fmt.Errorf("MinBars=%d 与生产因子窗口 %d 不一致：IC 必须度量选股器真正使用的因子",
			c.MinBars, screener.BarWindow())
	}
	if c.Step < 0 {
		return fmt.Errorf("Step 不能为负")
	}
	return nil
}

// Horizon 观察期（交易日）。
var Horizons = []int{5, 10, 20}

// FactorIC 单因子的 IC 统计。
type FactorIC struct {
	Name    string
	Horizon int
	N       int     // 有效样本的截面数
	ICMean  float64 // RankIC 均值
	ICStd   float64 // RankIC 标准差
	ICIR    float64 // ICMean / ICStd（>0.3 视为可用）
	ICPos   float64 // RankIC > 0 的比例（方向稳定性）
	// LastIC 最近一个截面的 RankIC（供观察近期是否失效）。
	LastIC float64
}

// Usable 该因子在本观察期是否达到"可作为选股依据"的经验门槛。
//
// 门槛取自通行做法（Qlib 工作流里的常用判据）：|ICMean| ≥ 0.03 且 |ICIR| ≥ 0.3。
// ICMean 取绝对值：A 股月频反转下，方向为负的因子同样可用（反向使用即可），
// 关键是有稳定的预测力，而不是"必须是正的"。
func (f FactorIC) Usable() bool {
	return f.N >= 30 && math.Abs(f.ICMean) >= 0.03 && math.Abs(f.ICIR) >= 0.3
}

// Result IC 度量总结果。
type Result struct {
	From, To   string
	TradeDates int // 参与计算的截面数（去重后）
	Codes      int
	ICs        []FactorIC
	// Forward 各观察期的平均覆盖（有多少截面能算出远期收益）。
	Forward map[int]int
}

// Find 取某因子在某观察期的统计；不存在返回零值与 false。
func (r *Result) Find(name string, horizon int) (FactorIC, bool) {
	for _, f := range r.ICs {
		if f.Name == name && f.Horizon == horizon {
			return f, true
		}
	}
	return FactorIC{}, false
}

// Config IC 计算参数。
type Config struct {
	MinBars int // 因子窗口根数（必须与 screener.BarWindow 一致，见 validate）
	// MinCodes 单截面的最小样本数：少于它该截面不参与（样本太小的 IC 是噪声）。
	MinCodes int
	// Step 采样步长（每 N 个交易日算一个截面）。1 = 每个交易日都算。
	Step int
	// Universe 非 nil 时只在通过这些硬门槛的可投池上算 IC（流动性/估值/价格）。
	// 强烈建议开：全市场算出的 IC 会被微盘股污染，与选股器实际面对的池子相差很大。
	Universe *screener.FilterConfig
	// From / To 限定截面日期区间（含端点，空 = 不限）。用于样本外检验：
	// 把历史切成两段分别算 IC，看结论是否稳定——单一样本内挑出的方向不算证据。
	From string
	To   string
}

// DefaultConfig 与生产同口径的默认参数。
func DefaultConfig() Config {
	return Config{MinBars: screener.BarWindow(), MinCodes: 50, Step: 1}
}

// RunIC 在历史数据上计算各因子的 RankIC 序列。
//
// 口径（无前视偏差）：
//   - 截面日 T 的因子只用 ≤T 的日线（窗口 T 往前 MinBars 根）与 T 日估值；
//   - 远期收益 = close(T+h) / close(T) − 1，h 取 Horizons。
//
// 因子值一律经 screener 的同一批函数计算，保证度量的是生产选股器真正使用的东西。
func RunIC(ctx context.Context, h *History, cfg Config) (Result, error) {
	if cfg.MinBars <= 0 {
		cfg.MinBars = 20
	}
	if cfg.MinCodes <= 0 {
		cfg.MinCodes = 50
	}
	if cfg.Step <= 0 {
		cfg.Step = 1
	}
	if err := cfg.validate(); err != nil {
		return Result{}, err
	}
	res := Result{Forward: map[int]int{}}
	if len(h.Dates) == 0 {
		return res, fmt.Errorf("历史为空：先跑 backfill 回补，或用 Load 读入 CSV.gz")
	}
	h.Finalize()
	res.From, res.To = h.Dates[0], h.Dates[len(h.Dates)-1]
	if cfg.From != "" {
		res.From = cfg.From
	}
	if cfg.To != "" {
		res.To = cfg.To
	}
	res.Codes = len(h.Codes())

	// 每个观察期各自一条 IC 序列。
	type series struct {
		ics  []float64
		last float64
	}
	acc := map[string]map[int]*series{}
	for _, name := range FactorNames {
		acc[name] = map[int]*series{}
		for _, hz := range Horizons {
			acc[name][hz] = &series{}
		}
	}
	datesUsed := 0

	for di := 0; di < len(h.Dates); di += cfg.Step {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		date := h.Dates[di]
		if cfg.From != "" && date < cfg.From {
			continue
		}
		if cfg.To != "" && date > cfg.To {
			continue
		}
		window := windowDates(h.Dates, di, cfg.MinBars)
		if len(window) < cfg.MinBars {
			continue // 窗口不足（历史开头），跳过
		}
		codes, fvals := crossSection(h, window, date, cfg)
		if len(codes) < cfg.MinCodes {
			continue
		}
		datesUsed++

		for _, hz := range Horizons {
			j := di + hz
			if j >= len(h.Dates) {
				break // 尚未到期
			}
			target := h.Dates[j]
			// 截面收益：同一天同一批代码的前复权收益。
			fwd := make([]float64, 0, len(codes))
			keep := make([]int, 0, len(codes))
			for i, c := range codes {
				base := h.Closes[date][c]
				end := h.Closes[target][c]
				if base <= 0 || end <= 0 {
					continue
				}
				fwd = append(fwd, end/base-1)
				keep = append(keep, i)
			}
			if len(fwd) < cfg.MinCodes {
				continue
			}
			res.Forward[hz]++

			for _, name := range FactorNames {
				vals := make([]float64, 0, len(keep))
				for _, i := range keep {
					vals = append(vals, fvals[name][i])
				}
				ic, ok := RankIC(vals, fwd)
				if !ok {
					continue // 该因子在这一截面无有效值
				}
				s := acc[name][hz]
				s.ics = append(s.ics, ic)
				s.last = ic
			}
		}
	}

	res.TradeDates = datesUsed
	for _, name := range FactorNames {
		for _, hz := range Horizons {
			s := acc[name][hz]
			res.ICs = append(res.ICs, summarize(name, hz, s.ics, s.last))
		}
	}
	return res, nil
}

// crossSection 计算某截面日全部可用标的的因子值。
//
// 返回：标的列表、各因子的原始取值切片（键为 FactorNames，值与该截面的 codes 同序）。
func crossSection(h *History, window []string, date string, cfg Config) ([]string, map[string][]float64) {
	valsForDate := h.Vals[date]
	codes := make([]string, 0, len(valsForDate))
	for code := range valsForDate {
		close, ok := h.Closes[date][code]
		if !ok || close <= 0 {
			continue
		}
		// 可投池近似：把生产漏斗里只看估值与价格的硬门槛套上，
		// 避免微盘股（生产已按流通市值剔除）污染因子 IC。
		if cfg.Universe != nil {
			v := valsForDate[code]
			stk := model.StockBasic{
				TsCode: code, ValDate: date,
				TurnoverRate: v.TurnoverRate, PETtm: v.PETtm, PB: v.PB, CircMvW: v.CircMvW,
			}
			// 价格门槛用**未复权**收盘：生产 valuationStage 的 price 契约就是当日未复权价
			// （screener 传入 raws 的最后一根）。用前复权价会让历史早年的价格上下界判定漂移。
			if !screener.HardFilters(stk, model.FromFloat(h.Raws[date][code]), *cfg.Universe) {
				continue
			}
		}
		codes = append(codes, code)
	}
	sort.Strings(codes)
	if len(codes) == 0 {
		return nil, nil
	}

	raw := make(map[string]screener.RawMetrics, len(codes))
	pePB := make(map[string][2]float64, len(codes))
	// 窗口包含截面日，所以最后一个交易日的成交量比就是当日量比口径的基础。
	// 窗口根数取自 screener（与生产因子窗口同一个常量），不用 cfg.MinBars：
	// 后者只决定"够不够算"，若二者不一致，度量的就不是生产在用的那个因子。
	for _, code := range codes {
		v := valsForDate[code]
		raw[code] = screener.ComputeRaw(h.WindowCloses(code, window), v.TurnoverRate, screener.BarWindow())
		pePB[code] = [2]float64{v.PETtm, v.PB}
	}
	fs := screener.BuildFactorScores(codes, raw, pePB)

	out := map[string][]float64{}
	// 两个综合分是**固定**的两个方向（原始 vs 反向），不随当前配置移动：
	// 若按"当前配置 + 反向"取值，默认翻成 reversal 后两者会算成同一个公式，
	// A/B 对照直接塌缩成一行相同数字（曾如此）。标签由 CLI 按当前模式生成。
	for _, name := range FactorNames {
		col := make([]float64, len(codes))
		for i, c := range codes {
			switch name {
			case "momentum":
				col[i] = fs[c].Momentum
			case "value":
				col[i] = fs[c].Value
			case "lowvol":
				col[i] = fs[c].LowVol
			case "liquidity":
				col[i] = fs[c].Liquidity
			case "composite":
				col[i] = screener.Composite(fs[c], screener.DefaultWeights())
			case "composite_rev":
				col[i] = screener.Composite(fs[c], screener.ReversalWeights())
			}
		}
		out[name] = col
	}
	return codes, out
}

// windowDates 取截至 di（含）的最近 n 个交易日，升序。
func windowDates(dates []string, di, n int) []string {
	start := di - n + 1
	if start < 0 {
		start = 0
	}
	out := make([]string, 0, di-start+1)
	for i := start; i <= di; i++ {
		out = append(out, dates[i])
	}
	return out
}

// RankIC 计算秩相关（Spearman）：先把两个序列转成平均秩，再取 Pearson 相关。
//
// 用秩而非原值：A 股收益分布厚尾、因子值多极值，Pearson 会被少数极端值主导；
// 秩相关对单调变换不变，是因子研究里的通行口径（Qlib 的 Rank IC 同此）。
// 任一序列全为同一值（无区分度）时返回 ok=false。
func RankIC(factor, forward []float64) (float64, bool) {
	n := len(factor)
	if n != len(forward) || n < 3 {
		return 0, false
	}
	rf, ok1 := averageRanks(factor)
	rr, ok2 := averageRanks(forward)
	if !ok1 || !ok2 {
		return 0, false
	}
	return pearson(rf, rr)
}

// averageRanks 返回平均秩（并列取平均），第二个返回值为 false 表示"全部并列、无区分度"。
func averageRanks(v []float64) ([]float64, bool) {
	n := len(v)
	if n == 0 {
		return nil, false
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return v[order[a]] < v[order[b]] })
	ranks := make([]float64, n)
	for i := 0; i < n; {
		j := i
		for j+1 < n && v[order[j+1]] == v[order[i]] {
			j++
		}
		avg := float64(i+j) / 2
		for k := i; k <= j; k++ {
			ranks[order[k]] = avg
		}
		i = j + 1
	}
	// 排序后首尾不等即存在区分度；首尾相等则全部并列。
	return ranks, v[order[0]] != v[order[n-1]]
}

// pearson 皮尔逊相关系数；任一侧方差为 0 返回 ok=false。
func pearson(x, y []float64) (float64, bool) {
	n := float64(len(x))
	if n == 0 {
		return 0, false
	}
	mx, my := 0.0, 0.0
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	mx /= n
	my /= n
	var sxy, sxx, syy float64
	for i := range x {
		dx, dy := x[i]-mx, y[i]-my
		sxy += dx * dy
		sxx += dx * dx
		syy += dy * dy
	}
	if sxx <= 0 || syy <= 0 {
		return 0, false
	}
	return sxy / math.Sqrt(sxx*syy), true
}

// summarize 把一条 IC 序列汇总成统计量。
func summarize(name string, hz int, ics []float64, last float64) FactorIC {
	f := FactorIC{Name: name, Horizon: hz, N: len(ics), LastIC: last}
	if len(ics) == 0 {
		return f
	}
	mean := 0.0
	for _, v := range ics {
		mean += v
	}
	mean /= float64(len(ics))
	f.ICMean = mean
	if len(ics) > 1 {
		var ss float64
		for _, v := range ics {
			ss += (v - mean) * (v - mean)
		}
		f.ICStd = math.Sqrt(ss / float64(len(ics)-1))
		if f.ICStd > 0 {
			f.ICIR = mean / f.ICStd
		}
	}
	pos := 0
	for _, v := range ics {
		if v > 0 {
			pos++
		}
	}
	f.ICPos = float64(pos) / float64(len(ics))
	return f
}

// Describe 单因子单观察期的可读摘要。
func (f FactorIC) Describe() string {
	verdict := "不足以作为依据"
	if f.Usable() {
		verdict = "可用"
	} else if f.N < 30 {
		verdict = "样本不足"
	}
	return fmt.Sprintf("%-10s h=%-2d n=%4d  IC=%+.4f  ICIR=%+.3f  正IC占比=%.0f%%  最近IC=%+.4f  → %s",
		f.Name, f.Horizon, f.N, f.ICMean, f.ICIR, f.ICPos*100, f.LastIC, verdict)
}

// WeightsFor 按 IC 结论给出权重建议（不自动应用，交人决定）。
//
// **权重带符号**：因子分是"越大越 X"的截面百分位（动量=涨幅高、低波=波动低…），
// 而 IC 为负意味着这个方向与未来收益相反——正确的用法是给它**负权重**（等于反向使用），
// 不是"权重归 0"。若只按 |IC| 归一而丢掉符号，会给负 IC 的因子继续派正权重，
// 综合分照旧反向，与"修好选股"的目标正好相反。
//
// 归一化：Σ|w| = 1；符号 = IC 符号；幅度 = |ICMean| 的占比。
// 只纳入达到可用门槛的因子；一个都没有时返回空。
func (r *Result) WeightsFor(horizon int) map[string]float64 {
	total := 0.0
	raw := map[string]float64{}
	for _, name := range FactorNames {
		if name == "composite" || name == "composite_rev" {
			continue
		}
		f, ok := r.Find(name, horizon)
		if !ok || !f.Usable() {
			continue
		}
		raw[name] = f.ICMean // 带符号
		total += math.Abs(f.ICMean)
	}
	out := map[string]float64{}
	if total <= 0 {
		return out
	}
	for k, v := range raw {
		out[k] = v / total
	}
	return out
}
