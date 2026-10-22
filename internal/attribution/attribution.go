// Package attribution 轻量候选归因：只读既有留痕与日线，回答"当时批/不批的候选，
// 后来 5/10/20 个交易日表现如何"。
//
// 不恢复旧的 review/backtest：不重新选股、不重新问模型、不写库。数据来源只有两处：
//   - run_trace 的 llm:<标的>:decision 决策留痕（ListLLMDecisions）；
//   - run_trace 的 screen:topn 每日候选快照（ListScreenTopN，含当日 score/factors）。
//
// 收益口径：信号日收盘买入、第 N 个交易日收盘卖出，扣双边佣金（每笔最低 5 元）、
// 卖出印花税与过户费；相对基准为沪深 300 同期涨幅。实际执行通常在次日，滑点未计，
// 所以它衡量的是"决策方向"而不是"成交回执"。
package attribution

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
)

// 裁决分桶名（买/跳过/评审失败）。失败不是"不该买"，不能混进 skip。
const (
	VerdictBuy    = "买入"
	VerdictSkip   = "跳过"
	VerdictFailed = "评审失败"

	// verdictBuy/verdictSkip 是 run_trace 里决策行的原始取值（llm 包的常量不导出，
	// 这里按落库契约用字面量；改值时由归因测试兜住）。
	verdictBuy  = "buy"
	verdictSkip = "skip"
)

// IndexCode 相对收益基准（与 store.MarketIndex 同值；归因只读 daily_bar）。
const IndexCode = "000300.SH"

// DefaultHorizons 默认前瞻窗口（交易日）。
var DefaultHorizons = []int{5, 10, 20}

// Options 一次归因的输入区间与窗口。
type Options struct {
	From, To string
	Horizons []int
}

// HorizonReturn 单条候选在某个前瞻窗口的结果。
type HorizonReturn struct {
	Completed bool    // 买卖两端日线齐全，Net 可用
	Pending   bool    // 目标日尚未到期（日历/行情还没走到）
	Missing   bool    // 已到期但缺日线（停牌/数据缺口）
	HasIndex  bool    // 基准两端日线齐全，Excess 可用
	Net       float64 // 扣双边成本后的净收益（小数）
	Index     float64 // 沪深300 同期收益（小数）
	Excess    float64 // Net − Index
}

// Row 一条决策留痕 + 候选快照。
type Row struct {
	TradeDate  string
	TsCode     string
	Name       string
	Industry   string
	Verdict    string
	Confidence float64
	WeightPct  float64
	Rank       int
	Score      float64
	Factors    model.FactorScore
	CloseFen   int64
	// NotionalFen 收益估算用的买入额：买入行按 weight×总资产（不超过单票上限），
	// 跳过/失败行按当日单票上限（等风险反事实），缺快照时回落到 5000 元参考额。
	NotionalFen model.Fen
	// EntryMissing 信号日没有该标的收盘价，所有窗口都无法计算。
	EntryMissing bool
	Returns      map[int]HorizonReturn
}

// HorizonResult 一个分组在一个窗口上的汇总。
type HorizonResult struct {
	Horizon   int
	N         int // 完成计算的样本数
	IndexN    int // 其中基准也可用的样本数
	Pending   int
	Missing   int
	AvgNet    float64
	AvgExcess float64
	WinRate   float64 // Net > 0 占比
	BeatRate  float64 // Excess > 0 占比（分母 IndexN）
}

// Bucket 一个分层（买入/跳过/置信度/行业）。
type Bucket struct {
	Key     string
	Rows    int
	Results []HorizonResult
}

// Report 一次归因的完整结果。
type Report struct {
	From, To      string
	Horizons      []int
	Decisions     int
	Reconstructed int // 无 decision 留痕、从证据行还原的失败候选数
	Snapshots     int
	Rows          []Row
	Overall       []HorizonResult
	ByVerdict     []Bucket
	ByConfidence  []Bucket
	ByIndustry    []Bucket
	Notes         []string
}

// Compute 执行归因。只读数据库，不写任何表。
func Compute(ctx context.Context, st *store.Store, opts Options) (*Report, error) {
	if opts.From == "" || opts.To == "" {
		return nil, fmt.Errorf("归因区间不能为空")
	}
	if opts.From > opts.To {
		return nil, fmt.Errorf("归因起始日 %s 晚于结束日 %s", opts.From, opts.To)
	}
	horizons := normalizeHorizons(opts.Horizons)
	maxH := horizons[len(horizons)-1]

	decisions, err := st.TraceRepo().ListLLMDecisions(ctx, opts.From, opts.To)
	if err != nil {
		return nil, err
	}
	snaps, err := st.TraceRepo().ListScreenTopN(ctx, opts.From, opts.To)
	if err != nil {
		return nil, err
	}
	codesByDate, err := st.TraceRepo().ListLLMCodes(ctx, opts.From, opts.To)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		From: opts.From, To: opts.To, Horizons: horizons,
		Decisions: len(decisions), Snapshots: len(snaps),
	}
	rep.Notes = append(rep.Notes,
		"净收益按信号日收盘买入、第 N 个交易日收盘卖出估算，已扣双边佣金(最低5元/笔)、卖出印花税0.1%与过户费；实际执行通常在次日，滑点未计。")
	if len(snaps) == 0 {
		rep.Notes = append(rep.Notes, "区间内没有候选快照（功能上线前的留痕），score/factors 不可用，行业与名称取自 stock_basic。")
	}

	snapByDate := make(map[string]map[string]model.CandidateSnapshot, len(snaps))
	for date, snap := range snaps {
		m := make(map[string]model.CandidateSnapshot, len(snap.Candidates))
		for _, c := range snap.Candidates {
			m[c.TsCode] = c
		}
		snapByDate[date] = m
	}

	decisionCodes := make(map[string]map[string]bool, len(decisions))
	rows := make([]Row, 0, len(decisions))
	codes := make(map[string]bool, len(decisions))
	needBrief := make(map[string]bool)
	for _, d := range decisions {
		set := decisionCodes[d.TradeDate]
		if set == nil {
			set = make(map[string]bool)
			decisionCodes[d.TradeDate] = set
		}
		set[d.TsCode] = true
		r := Row{
			TradeDate: d.TradeDate, TsCode: d.TsCode, Verdict: verdictOf(d),
			Confidence: d.Confidence, WeightPct: d.WeightPct,
			Returns: make(map[int]HorizonReturn, len(horizons)),
		}
		if c, ok := snapByDate[d.TradeDate][d.TsCode]; ok {
			r.Name, r.Industry, r.Rank, r.Score, r.CloseFen = c.Name, c.Industry, c.Rank, c.Score, c.CloseFen
			r.Factors = model.FactorScore{Momentum: c.Momentum, Value: c.Value,
				LowVol: c.LowVol, Liquidity: c.Liquidity}
		}
		if r.Name == "" || r.Industry == "" {
			needBrief[d.TsCode] = true
		}
		rows = append(rows, r)
		codes[d.TsCode] = true
	}

	// 没有 decision 结论的候选：整批证据失败（10-09 的 DSML 泄漏）或部分候选证据不全。
	// 从证据行的 subject 还原代码，按"评审失败"计入前瞻收益——失败不是"跳过"，
	// 收益是"如果当时批了/没批"的反事实，bucket 名字必须说清楚。
	for date, list := range codesByDate {
		for _, code := range list {
			if decisionCodes[date][code] {
				continue
			}
			rows = append(rows, Row{
				TradeDate: date, TsCode: code, Verdict: VerdictFailed,
				Returns: make(map[int]HorizonReturn, len(horizons)),
			})
			needBrief[code] = true
			codes[code] = true
			rep.Reconstructed++
		}
	}
	if rep.Reconstructed > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d 条候选没有 decision 结论（整批/部分评审失败），已从证据行还原并按“评审失败”计入。",
			rep.Reconstructed))
	}
	if len(rows) == 0 {
		rep.Notes = append(rep.Notes, "区间内没有 LLM 决策留痕，也没有可还原的证据行。")
		return rep, nil
	}
	if len(needBrief) > 0 {
		briefs, err := st.ScreenRepo().StockBriefs(ctx, sortedKeys(needBrief))
		if err != nil {
			return nil, err
		}
		for i := range rows {
			b, ok := briefs[rows[i].TsCode]
			if !ok {
				continue
			}
			if rows[i].Name == "" {
				rows[i].Name = b.Name
			}
			if rows[i].Industry == "" {
				rows[i].Industry = b.Industry
			}
		}
	}
	for i := range rows {
		if rows[i].Name == "" {
			rows[i].Name = rows[i].TsCode
		}
		if rows[i].Industry == "" {
			rows[i].Industry = "未知"
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].TradeDate != rows[j].TradeDate {
			return rows[i].TradeDate < rows[j].TradeDate
		}
		return rows[i].TsCode < rows[j].TsCode
	})

	// 前瞻交易日：按决策日逐日取，避免自然日加法把周末/节假日算进去。
	forward := make(map[string][]string)
	dateSet := make(map[string]bool)
	for _, r := range rows {
		if _, ok := forward[r.TradeDate]; ok {
			continue
		}
		dates, err := st.ScreenRepo().NextTradeDates(ctx, r.TradeDate, maxH)
		if err != nil {
			return nil, err
		}
		forward[r.TradeDate] = dates
		dateSet[r.TradeDate] = true
		for _, d := range dates {
			dateSet[d] = true
		}
	}

	bars, err := loadBars(ctx, st, sortedKeys(codes), sortedKeys(dateSet))
	if err != nil {
		return nil, err
	}
	today := time.Now().Format("20060102")
	for i := range rows {
		r := &rows[i]
		r.NotionalFen = notionalOf(*r, snaps[r.TradeDate])
		entry, ok := bars[r.TsCode][r.TradeDate]
		if !ok || rawFen(entry) <= 0 {
			r.EntryMissing = true
			continue
		}
		entryFen := rawFen(entry)
		idxEntry, idxOK := bars[IndexCode][r.TradeDate]
		for _, h := range horizons {
			hr := HorizonReturn{}
			fwd := forward[r.TradeDate]
			if len(fwd) < h {
				hr.Pending = true
				r.Returns[h] = hr
				continue
			}
			target := fwd[h-1]
			exit, ok := bars[r.TsCode][target]
			if !ok || rawFen(exit) <= 0 {
				hr.Pending, hr.Missing = horizonStatus(target, today, false)
				r.Returns[h] = hr
				continue
			}
			hr.Completed = true
			hr.Net = netReturn(model.Fen(entryFen), model.Fen(rawFen(exit)), r.NotionalFen)
			if idxOK && idxEntry.Close > 0 {
				if idxExit, ok := bars[IndexCode][target]; ok && idxExit.Close > 0 {
					hr.Index = idxExit.Close/idxEntry.Close - 1
					hr.Excess = hr.Net - hr.Index
					hr.HasIndex = true
				}
			}
			r.Returns[h] = hr
		}
	}

	rep.Rows = rows
	rep.Overall = aggregate(rows, horizons)
	rep.ByVerdict = bucketize(rows, horizons, func(r Row) string { return r.Verdict },
		[]string{VerdictBuy, VerdictSkip, VerdictFailed})
	rep.ByConfidence = bucketize(rows, horizons, confidenceBucket,
		[]string{"<0.60", "0.60-0.70", "0.70-0.80", "≥0.80", "未给置信"})
	rep.ByIndustry = bucketize(rows, horizons, func(r Row) string { return r.Industry }, nil)
	appendNotes(rep)
	return rep, nil
}

// SummaryLine 给 M5 的一行摘要：近 lookbackDays 个交易日、指定窗口的买入/跳过/失败对照。
// 无留痕或窗口未到期时返回可读的一句话，不返回错误（日报不能因为归因没数据而失败）。
func SummaryLine(ctx context.Context, st *store.Store, endDate string, lookbackDays, horizon int) (string, error) {
	if lookbackDays <= 0 {
		lookbackDays = 20
	}
	dates, err := st.ScreenRepo().WindowDates(ctx, endDate, lookbackDays)
	if err != nil {
		return "", err
	}
	if len(dates) == 0 {
		return "归因：区间内无交易日", nil
	}
	rep, err := Compute(ctx, st, Options{From: dates[0], To: endDate, Horizons: []int{horizon}})
	if err != nil {
		return "", err
	}
	if rep.Decisions == 0 && rep.Reconstructed == 0 {
		return fmt.Sprintf("近 %d 个交易日无 LLM 决策留痕，暂无归因", len(dates)), nil
	}
	parts := make([]string, 0, 3)
	for _, key := range []string{VerdictBuy, VerdictSkip, VerdictFailed} {
		b := findBucket(rep.ByVerdict, key)
		if b == nil {
			continue
		}
		hr := b.Results[0]
		if hr.N == 0 {
			parts = append(parts, fmt.Sprintf("%s %d 只（待到期 %d/缺数据 %d）",
				key, b.Rows, hr.Pending, hr.Missing))
			continue
		}
		ex := ""
		if hr.IndexN > 0 {
			ex = fmt.Sprintf("、超额 %+.2f%%", hr.AvgExcess*100)
		}
		parts = append(parts, fmt.Sprintf("%s %d 只 净收益 %+.2f%%%s（n=%d）",
			key, b.Rows, hr.AvgNet*100, ex, hr.N))
	}
	return fmt.Sprintf("近 %d 个交易日候选归因（%d 日，扣双边成本）：%s。",
		len(dates), horizon, strings.Join(parts, "；")), nil
}

// loadBars 分批读取代码 × 日期的日线，返回 code→date→point。
func loadBars(ctx context.Context, st *store.Store, codes, dates []string) (map[string]map[string]store.ClosePoint, error) {
	out := make(map[string]map[string]store.ClosePoint, len(codes)+1)
	if len(codes) == 0 || len(dates) == 0 {
		return out, nil
	}
	all := make([]string, 0, len(codes)+1)
	all = append(all, codes...)
	all = append(all, IndexCode)
	const chunk = 400
	for start := 0; start < len(all); start += chunk {
		end := min(start+chunk, len(all))
		pts, err := st.ScreenRepo().BarsForCodes(ctx, all[start:end], dates)
		if err != nil {
			return nil, err
		}
		for _, p := range pts {
			m := out[p.TsCode]
			if m == nil {
				m = make(map[string]store.ClosePoint)
				out[p.TsCode] = m
			}
			m[p.TradeDate] = p
		}
	}
	return out, nil
}

// notionalOf 收益估算的买入额。买入行用模型给的 weight×总资产（不超过单票上限）；
// 跳过/失败行用当日单票上限做等风险反事实；没有快照的旧留痕用 5000 元参考额。
func notionalOf(r Row, snap model.ScreenSnapshot) model.Fen {
	if snap.TotalAssetFen > 0 && r.WeightPct > 0 {
		n := int64(float64(snap.TotalAssetFen) * r.WeightPct)
		if snap.SlotFen > 0 && n > snap.SlotFen {
			n = snap.SlotFen
		}
		if n > 0 {
			return model.Fen(n)
		}
	}
	if snap.SlotFen > 0 {
		return model.Fen(snap.SlotFen)
	}
	return risk.DefaultMinSingleAmountFen
}

// netReturn 扣双边成本的净收益率。amount 为买入金额（分）。
func netReturn(entry, exit, amount model.Fen) float64 {
	if entry <= 0 || amount <= 0 {
		return math.NaN()
	}
	qty := float64(amount) / float64(entry)
	buyAmt := float64(entry) * qty
	sellAmt := float64(exit) * qty
	buyFee := float64(risk.BuyFeeFen(model.Fen(math.Round(buyAmt))))
	sellFee := float64(risk.SellFeeFen(model.Fen(math.Round(sellAmt))))
	return (sellAmt - sellFee - buyAmt - buyFee) / (buyAmt + buyFee)
}

// rawFen 未复权收盘价（分）；缺失时回落到 close 列（个股同样是分）。
func rawFen(p store.ClosePoint) float64 {
	if p.RawClose > 0 {
		return p.RawClose
	}
	return p.Close
}

// horizonStatus 目标日无日线时的归类：目标日还没走完（含今天）是"待到期"，
// 过去的日期仍缺线才是"缺数据"（停牌/数据缺口）。今天的目标日到明天会自然转成后者。
func horizonStatus(target, today string, hasBar bool) (pending, missing bool) {
	if hasBar {
		return false, false
	}
	if target >= today {
		return true, false
	}
	return false, true
}

// verdictOf 决策留痕 → 归因分桶。非 ok 或无法识别的裁决按"评审失败"处理。
func verdictOf(d model.LLMCall) string {
	if d.Status != model.TraceOK {
		return VerdictFailed
	}
	switch d.Verdict {
	case verdictBuy:
		return VerdictBuy
	case verdictSkip:
		return VerdictSkip
	default:
		return VerdictFailed
	}
}

// confidenceBucket 置信度分层；失败行或未给置信的归到"未给置信"。
func confidenceBucket(r Row) string {
	if r.Verdict == VerdictFailed || r.Confidence <= 0 {
		return "未给置信"
	}
	switch {
	case r.Confidence < 0.60:
		return "<0.60"
	case r.Confidence < 0.70:
		return "0.60-0.70"
	case r.Confidence < 0.80:
		return "0.70-0.80"
	default:
		return "≥0.80"
	}
}

// aggregate 一个样本集在全部窗口上的汇总。
func aggregate(rows []Row, horizons []int) []HorizonResult {
	out := make([]HorizonResult, len(horizons))
	for i, h := range horizons {
		res := HorizonResult{Horizon: h}
		var sumNet, sumExcess float64
		var win, beat int
		for _, r := range rows {
			hr, ok := r.Returns[h]
			if !ok {
				res.Missing++ // 信号日无日线
				continue
			}
			if !hr.Completed {
				if hr.Pending {
					res.Pending++
				} else {
					res.Missing++
				}
				continue
			}
			res.N++
			sumNet += hr.Net
			if hr.Net > 0 {
				win++
			}
			if hr.HasIndex {
				res.IndexN++
				sumExcess += hr.Excess
				if hr.Excess > 0 {
					beat++
				}
			}
		}
		if res.N > 0 {
			res.AvgNet = sumNet / float64(res.N)
			res.WinRate = float64(win) / float64(res.N)
		}
		if res.IndexN > 0 {
			res.AvgExcess = sumExcess / float64(res.IndexN)
			res.BeatRate = float64(beat) / float64(res.IndexN)
		}
		out[i] = res
	}
	return out
}

// bucketize 按 keyFn 分层聚合；order 指定展示顺序，未列出的键按样本数降序。
func bucketize(rows []Row, horizons []int, keyFn func(Row) string, order []string) []Bucket {
	groups := make(map[string][]Row)
	for _, r := range rows {
		k := keyFn(r)
		groups[k] = append(groups[k], r)
	}
	out := make([]Bucket, 0, len(groups))
	for k, rs := range groups {
		out = append(out, Bucket{Key: k, Rows: len(rs), Results: aggregate(rs, horizons)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, oj := orderIndex(order, out[i].Key), orderIndex(order, out[j].Key)
		if oi != oj {
			return oi < oj
		}
		if out[i].Rows != out[j].Rows {
			return out[i].Rows > out[j].Rows
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func orderIndex(order []string, key string) int {
	for i, k := range order {
		if k == key {
			return i
		}
	}
	return len(order)
}

func findBucket(buckets []Bucket, key string) *Bucket {
	for i := range buckets {
		if buckets[i].Key == key {
			return &buckets[i]
		}
	}
	return nil
}

func appendNotes(rep *Report) {
	entryMissing := 0
	for _, r := range rep.Rows {
		if r.EntryMissing {
			entryMissing++
		}
	}
	if entryMissing > 0 {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"%d 条决策在信号日没有收盘价（停牌/数据缺口），未计入收益。", entryMissing))
	}
	pendingAny := false
	for _, hr := range rep.Overall {
		if hr.N == 0 {
			rep.Notes = append(rep.Notes, fmt.Sprintf(
				"%d 日窗口暂无可计算样本（待到期 %d、缺数据 %d）。", hr.Horizon, hr.Pending, hr.Missing))
		}
		if hr.Pending > 0 {
			pendingAny = true
		}
	}
	if pendingAny {
		rep.Notes = append(rep.Notes, "待到期样本会随交易日推进自动补齐，不需要重跑历史。")
	}
}

func normalizeHorizons(in []int) []int {
	out := make([]int, 0, len(in))
	seen := make(map[int]bool, len(in))
	for _, h := range in {
		if h <= 0 || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	if len(out) == 0 {
		out = append(out, DefaultHorizons...)
	}
	sort.Ints(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
