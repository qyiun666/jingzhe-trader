// Package review 决策归因：把每条买入决策与它的后续实际收益对上，回答
// "模型自报的置信度到底有没有预测力"。
//
// 存在理由（对照外部实践）：当前决策链里 LLM 的 Confidence 是唯一软门槛依据
// （G1 档 0.55 以下不买，见 risk.GearTable），但这个数字从未与真实收益对照过，
// 门槛高低因此无从校准。多 agent 交易框架的消融实验普遍显示，贡献最大的组件
// 不是辩论环节而是**交易后的反思闭环**（取实际收益、生成反思、注入未来上下文）——
// 本包先做那个闭环里最不可省的一步：把结果如实记下来。
//
// 依赖方向：review 依赖 store 与 model，不触网（数据全来自本地 daily_bar）。
package review

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// Horizons 决策后的观察期（交易日偏移）。5/10/20 覆盖系统 20 日因子窗口与
// 最短持仓周期：5 日看短线是否立刻反向，20 日看是否兑现。
var Horizons = []int{5, 10, 20}

// Calibrator 决策归因器：读决策留痕 → 补后续收益 → 落归因行 → 出统计。
type Calibrator struct {
	st  *store.Store
	now func() time.Time
}

// NewCalibrator 构造归因器。
func NewCalibrator(st *store.Store) *Calibrator {
	return &Calibrator{st: st, now: time.Now}
}

// WithClock 注入时钟（测试用）。
func (c *Calibrator) WithClock(f func() time.Time) *Calibrator {
	if f != nil {
		c.now = f
	}
	return c
}

// Stats 校准统计：按置信度分层看方向命中率。
//
// 只有 Decision 与 buy 组参与"胜率"统计 —— skip 的票事后上涨同样有意义
// （说明模型错过），故单独成组，不混进胜率里虚高。
type Stats struct {
	Total      int       // 参与统计的归因行（含已到期收益）
	Buys       int       // 其中裁决为 buy 的
	BuyWinRate float64   // buy 组 20 日正收益占比
	BuyAvgRet  float64   // buy 组 20 日平均收益
	HighConf   GroupStat // confidence ≥ 阈值
	LowConf    GroupStat // confidence < 阈值
	Missed     int       // skip 但事后上涨（模型错过）的只数
	Horizon    int       // 本统计用的观察期
}

// GroupStat 单个置信度分组的统计。
type GroupStat struct {
	N       int
	WinRate float64
	AvgRet  float64
}

// ConfidenceCut 高/低置信度分界：即 G1 档的置信度门槛语义位置（0.70 = G2 门槛）。
// 取 0.70 而非 0.55 是为了让两组都有足够样本：0.55~0.70 与 ≥0.70。
const ConfidenceCut = 0.70

// Run 执行一轮归因：补算到期收益并返回统计。
//
// lookback 为回溯的决策天数（自然日）；超出 daily_bar 保留窗口（默认 45 天）的
// 决策算不出收益，由调用方用合理窗口（默认 40 天）避开。
func (c *Calibrator) Run(ctx context.Context, asOf string, lookbackDays int) (Stats, error) {
	if lookbackDays <= 0 {
		lookbackDays = defaultLookbackDays
	}
	days, err := c.st.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return Stats{}, err
	}
	if len(days) == 0 {
		return Stats{}, errors.New("交易日历为空，无法归因（先跑 calendar）")
	}
	idx := make(map[string]int, len(days))
	for i, d := range days {
		idx[d] = i
	}
	from := shiftDate(asOf, -lookbackDays)
	if from > asOf {
		from = asOf
	}

	decisions, err := c.st.TraceRepo().ListLLMDecisions(ctx, from, asOf)
	if err != nil {
		return Stats{}, err
	}
	if len(decisions) == 0 {
		return Stats{}, nil
	}

	// 一次读齐涉及代码的日线，避免逐条查询（决策数 × 观察期 = 查询爆炸）。
	closes, err := c.loadCloses(ctx, decisions, days, idx, asOf)
	if err != nil {
		return Stats{}, err
	}

	existing, err := c.st.TraceRepo().ListCalibrations(ctx, from, asOf)
	if err != nil {
		return Stats{}, err
	}
	have := make(map[string]model.Calibration, len(existing))
	for _, e := range existing {
		have[e.TradeDate+"|"+e.TsCode] = e
	}

	now := c.now().UTC().Format(time.RFC3339)
	rows := make([]model.Calibration, 0, len(decisions))
	for _, d := range decisions {
		row := model.Calibration{
			TradeDate: d.TradeDate, TsCode: d.TsCode,
			Verdict: d.Verdict, Confidence: d.Confidence, WeightPct: d.WeightPct, At: now,
		}
		base := closes[d.TsCode][d.TradeDate]
		// 已算出的收益不重算：历史决策的收益是既成事实，重算只会被复权口径变化带偏。
		prev := have[d.TradeDate+"|"+d.TsCode]
		for _, h := range Horizons {
			v, ok := forwardReturn(closes[d.TsCode], days, idx, d.TradeDate, h, base, asOf, prev)
			setReturn(&row, h, v, ok)
		}
		rows = append(rows, row)
		if err := c.st.TraceRepo().WriteCalibration(ctx, row); err != nil {
			return Stats{}, err
		}
	}
	return Summarize(rows), nil
}

// forwardReturn 计算决策日后第 h 个交易日的收益；不可算时返回 (0,false)。
//
// 不可算的两种情形都返回 false（不插值、不用最近价代）：目标交易日还没到，
// 或该票在决策日/目标日没有日线（停牌、退市）。
func forwardReturn(ser map[string]float64, days []string, idx map[string]int, base string,
	h int, baseClose float64, asOf string, prev model.Calibration) (float64, bool) {
	// 优先沿用已落库的值（既成事实不重算）。
	if v, ok := prevReturn(prev, h); ok {
		return v, true
	}
	if baseClose <= 0 {
		return 0, false
	}
	i, ok := idx[base]
	if !ok {
		return 0, false
	}
	j := i + h
	if j >= len(days) {
		return 0, false
	}
	target := days[j]
	if target > asOf {
		return 0, false // 尚未到期
	}
	tc, ok := ser[target]
	if !ok || tc <= 0 {
		return 0, false
	}
	return tc/baseClose - 1, true
}

// prevReturn 取已落库的观察期收益。
func prevReturn(p model.Calibration, h int) (float64, bool) {
	switch h {
	case 5:
		return p.Ret5, p.Has5
	case 10:
		return p.Ret10, p.Has10
	case 20:
		return p.Ret20, p.Has20
	}
	return 0, false
}

// setReturn 把 (值,有效性) 写回对应观察期字段。
func setReturn(row *model.Calibration, h int, v float64, ok bool) {
	switch h {
	case 5:
		row.Ret5, row.Has5 = v, ok
	case 10:
		row.Ret10, row.Has10 = v, ok
	case 20:
		row.Ret20, row.Has20 = v, ok
	}
}

// loadCloses 批量读取涉及代码在 [决策日下界, asOf] 的收盘序列（按代码 → 日期 → 收盘）。
func (c *Calibrator) loadCloses(ctx context.Context, decisions []model.LLMCall,
	days []string, idx map[string]int, asOf string) (map[string]map[string]float64, error) {
	codes := make([]string, 0, len(decisions))
	seen := make(map[string]bool, len(decisions))
	minDate := asOf
	for _, d := range decisions {
		if !seen[d.TsCode] {
			seen[d.TsCode] = true
			codes = append(codes, d.TsCode)
		}
		if d.TradeDate < minDate {
			minDate = d.TradeDate
		}
	}
	sort.Strings(codes)
	series, err := c.st.ScreenRepo().BarCloseSeries(ctx, tradeDatesBetween(days, minDate, asOf))
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(codes))
	for _, code := range codes {
		want[code] = true
	}
	out := make(map[string]map[string]float64, len(codes))
	for _, p := range series {
		if !want[p.TsCode] {
			continue
		}
		m := out[p.TsCode]
		if m == nil {
			m = map[string]float64{}
			out[p.TsCode] = m
		}
		m[p.TradeDate] = p.Close
	}
	return out, nil
}

// tradeDatesBetween 取 [from, to] 区间内的交易日（两端含）。
func tradeDatesBetween(days []string, from, to string) []string {
	var out []string
	for _, d := range days {
		if d >= from && d <= to {
			out = append(out, d)
		}
	}
	return out
}

// Summarize 把归因行汇总成分层统计（纯函数，可单测）。
func Summarize(rows []model.Calibration) Stats {
	const h = 20
	st := Stats{Horizon: h}
	var high, low GroupStat
	for _, r := range rows {
		if !r.Has20 {
			continue
		}
		st.Total++
		ret := r.Ret20
		if r.Verdict == "buy" {
			st.Buys++
			if ret > 0 {
				st.BuyWinRate += 1
			}
			st.BuyAvgRet += ret
			if r.Confidence >= ConfidenceCut {
				high.N++
				high.AvgRet += ret
				if ret > 0 {
					high.WinRate++
				}
			} else {
				low.N++
				low.AvgRet += ret
				if ret > 0 {
					low.WinRate++
				}
			}
		} else if ret > 0 {
			// 模型说不买、事后却涨了：错过。
			st.Missed++
		}
	}
	st.BuyWinRate = ratio(st.BuyWinRate, st.Buys)
	st.BuyAvgRet = avg(st.BuyAvgRet, st.Buys)
	high.WinRate = ratio(high.WinRate, high.N)
	high.AvgRet = avg(high.AvgRet, high.N)
	low.WinRate = ratio(low.WinRate, low.N)
	low.AvgRet = avg(low.AvgRet, low.N)
	st.HighConf, st.LowConf = high, low
	return st
}

// HasSignal 高/低置信度两组的胜率或均值差异是否达到"值得看"的量级。
// 两组各不足 minGroup 只票时一律返回 false：样本太小时任何差异都是噪声。
func (s Stats) HasSignal() bool {
	const minGroup = 8
	if s.HighConf.N < minGroup || s.LowConf.N < minGroup {
		return false
	}
	return math.Abs(s.HighConf.WinRate-s.LowConf.WinRate) >= 0.10 ||
		math.Abs(s.HighConf.AvgRet-s.LowConf.AvgRet) >= 0.02
}

// InconclusiveNote 两组样本都够、但差异未达显著量级时的说明句；不适用返回空串。
// 由日报与 CLI 共用，避免同一判断在两处各写一套文案。
func (s Stats) InconclusiveNote() string {
	if s.Total == 0 || s.HasSignal() || s.HighConf.N == 0 || s.LowConf.N == 0 {
		return ""
	}
	return "两组差异未达显著量级：置信度门槛暂不具区分度证据"
}

// defaultLookbackDays 默认回溯窗口（自然日）：须小于 daily_bar 个股保留窗口
// （默认 45 天），否则 20 日观察期的收益会被清理删掉而永远算不出来。
const defaultLookbackDays = 40

// shiftDate 把 YYYYMMDD 平移 n 天（n 为负表示往前）。
func shiftDate(date string, n int) string {
	t, err := time.ParseInLocation("20060102", date, time.UTC)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, n).Format("20060102")
}

func ratio(part float64, n int) float64 {
	if n <= 0 {
		return 0
	}
	return part / float64(n)
}

func avg(sum float64, n int) float64 {
	if n <= 0 {
		return 0
	}
	return sum / float64(n)
}

// Describe 单行摘要（日报与 CLI 共用，避免两处各写一套文案）。
func (s Stats) Describe() string {
	if s.Total == 0 {
		return "决策归因：尚无可统计样本（需决策后满 20 个交易日）"
	}
	return fmt.Sprintf("决策归因（观察期 %d 交易日，样本 %d 条 / 买入 %d 条）：买入胜率 %.0f%%、平均收益 %+.2f%%；"+
		"高置信(≥%.2f) %d 条 胜率 %.0f%% / 均 %+.2f%%，低置信 %d 条 胜率 %.0f%% / 均 %+.2f%%；模型错过（判不买而后上涨）%d 条",
		s.Horizon, s.Total, s.Buys, s.BuyWinRate*100, s.BuyAvgRet*100,
		ConfidenceCut, s.HighConf.N, s.HighConf.WinRate*100, s.HighConf.AvgRet*100,
		s.LowConf.N, s.LowConf.WinRate*100, s.LowConf.AvgRet*100, s.Missed)
}
