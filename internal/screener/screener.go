package screener

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// AlertCodeScreenEmpty 候选为 0 的 urgent 告警码（附录 B）。
const AlertCodeScreenEmpty = "SCREEN_EMPTY"

// momentumBars 动量/低波因子与板块强弱所需的日线根数（约一个月）。
const momentumBars = 20

// syncBackfillMargin 调度器每日回补余量：窗口 + 余量 = 应保证的交易日数，
// 缺口由 SyncDaily 按日补拉（每天一次调用即全市场，代价远低于逐只拉）。
const syncBackfillMargin = 5

// Screener 选股器：一条顺序流水线，产出内存候选，不写任何表。
type Screener struct {
	st  *store.Store
	cfg FilterConfig
	w   FactorWeights
}

// New 构造选股器。cfg 由组合根从 config screen.* 读出（默认值只有 KeySpec 一份）。
func New(st *store.Store, cfg FilterConfig) *Screener {
	return &Screener{st: st, cfg: cfg, w: DefaultWeights()}
}

// BarWindow 因子窗口所需交易日数（全项目最深的历史回看口径，其它模块以此为准）。
func BarWindow() int { return momentumBars }

// SyncBackDays 行情同步应保证的最近交易日数（选股是最深消费者）。
func (s *Screener) SyncBackDays() int { return momentumBars + syncBackfillMargin }

// Budget 单笔预算：可用现金按计划持仓数均分。Slots<=0 或无现金口径时返回 0（不放行）。
type Budget struct {
	Cash     model.Fen
	Slots    int
	MarketOK bool // 大盘是否允许开新仓（指数在 MA20 上方）
}

func (b Budget) perSlot() model.Fen {
	if b.Slots <= 0 || b.Cash <= 0 {
		return 0
	}
	return b.Cash / model.Fen(b.Slots)
}

// Report 一次选股运行的产出（供 CLI 打印与日志）。
type Report struct {
	TradeDate   string
	Candidates  []model.Candidate
	Sectors     []model.SectorStat
	Stages      []StageStat
	ScoredTotal int
	Empty       bool
	Notes       []string
}

// StageStat 漏斗单级进出统计（只写日志，不落库）。
// Slug 是进 artifacts 的 ASCII 键，Name 是日志与告警里的人读名。
type StageStat struct {
	Stage int
	Slug  string
	Name  string
	In    int
	Out   int
	Drops map[string]int
}

// inputs 一次选股读到的全部数据（窗口内截面）。
//
// stocks 是"每只票一行的当前快照"，静态属性与估值截面同在其中；
// closes/vols/raws 是因子窗口的升序序列，按代码索引。现价不另存，取 raws 的最后一根。
type inputs struct {
	dates     []string
	stocks    []model.StockBasic
	closes    map[string][]float64 // 前复权升序收盘（因子口径，比值无单位）
	vols      map[string][]float64 // 成交量（手）
	raws      map[string][]float64 // 未复权升序收盘（分）
	suspended map[string]bool
}

// price 当日一手价：因子窗口最后一根未复权收盘。没有日线（如停牌）返回 0，
// 由后续估值筛按"价格过低"淘汰——报不出价的票本来就不该下单。
func (in *inputs) price(tsCode string) model.Fen {
	r := in.raws[tsCode]
	if len(r) == 0 {
		return 0
	}
	return model.Fen(int64(r[len(r)-1] + 0.5))
}

// Run 执行选股流水线：板块强弱排名 → 资格 → 板块 → 可用资金 → 流动性 → 估值 → 因子排名 TopN。
// 数据全部读自本地缓存，本函数不触网；任何过程都不写库，只有候选为空时落一条告警。
func (s *Screener) Run(ctx context.Context, tradeDate string, budget Budget) (*Report, error) {
	in, err := s.load(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	rep := &Report{TradeDate: tradeDate}

	sectors := rankSectors(in, s.cfg)
	rep.Sectors = sectors
	hot := hotSectors(sectors, s.cfg.SectorTopK)
	rep.Notes = append(rep.Notes, "板块排名 "+strings.Join(topSectorNames(sectors, s.cfg.SectorTopK), "、"))

	tr := &tracer{rep: rep}
	survivors := s.filterStage(tr, "elig", "基础资格(ST/新股/停牌)", in.stocks, func(stk model.StockBasic) (bool, string) {
		return basicEligible(stk, listDaysBetween(stk.ListDate, tradeDate), s.cfg.MinListDays, in.suspended[stk.TsCode])
	})

	if !budget.MarketOK {
		rep.Empty = true
		tr.gate("regime", "大盘门槛(指数≥MA20)", survivors, reasonMarketRegime)
		return rep, s.finish(ctx, tradeDate, rep)
	}

	survivors = s.filterStage(tr, "sector", "板块强弱TopK", survivors, func(stk model.StockBasic) (bool, string) {
		return sectorGateStage(stk.Industry, hot)
	})
	survivors = s.filterStage(tr, "budget", "可用资金(一手≤预算)", survivors, func(stk model.StockBasic) (bool, string) {
		if !hasValuation(stk, tradeDate) {
			return false, reasonNoValuation
		}
		return affordableStage(in.price(stk.TsCode), budget.perSlot())
	})
	survivors = s.filterStage(tr, "liq", "流动性(市值/换手)", survivors, func(stk model.StockBasic) (bool, string) {
		if !hasValuation(stk, tradeDate) {
			return false, reasonNoValuation
		}
		return liquidityStage(stk, s.cfg)
	})
	survivors = s.filterStage(tr, "val", "估值(价格/PE/PB)", survivors, func(stk model.StockBasic) (bool, string) {
		if !hasValuation(stk, tradeDate) {
			return false, reasonNoValuation
		}
		return valuationStage(stk, in.price(stk.TsCode), s.cfg)
	})

	rep.ScoredTotal = len(survivors)
	rep.Candidates = s.pickTopN(survivors, in, sectors, tr)
	rep.Empty = len(rep.Candidates) == 0
	return rep, s.finish(ctx, tradeDate, rep)
}

// filterStage 通用一级筛选：逐只判定，计入 tracer。
func (s *Screener) filterStage(tr *tracer, slug, name string, pool []model.StockBasic,
	keep func(model.StockBasic) (bool, string)) []model.StockBasic {
	out := make([]model.StockBasic, 0, len(pool))
	for _, stk := range pool {
		if ok, why := keep(stk); ok {
			out = append(out, stk)
		} else {
			tr.drop(why)
		}
	}
	tr.emit(slug, name, len(pool), out)
	return out
}

// pickTopN 因子打分并取前 N 名，组装内存候选（含可解释理由）。
func (s *Screener) pickTopN(pool []model.StockBasic, in *inputs, sectors []model.SectorStat, tr *tracer) []model.Candidate {
	scored, raw := s.scorePool(pool, in)
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score != scored[j].Score {
			return scored[i].Score > scored[j].Score
		}
		return scored[i].Code < scored[j].Code
	})
	topN := s.cfg.TopN
	if topN > len(scored) {
		topN = len(scored)
	}
	sectorMom := make(map[string]float64, len(sectors))
	for _, sec := range sectors {
		sectorMom[sec.Industry] = sec.WMom
	}
	out := make([]model.Candidate, 0, topN)
	for rank, sc := range scored[:topN] {
		stk := stockOf(pool, sc.Code)
		out = append(out, model.Candidate{
			Rank: rank + 1, PoolSize: len(scored),
			TsCode: sc.Code, Name: stk.Name, Industry: stk.Industry,
			Score: sc.Score, Factors: sc.Factors, Close: in.price(sc.Code),
			CircMvW: stk.CircMvW, PETtm: stk.PETtm, PB: stk.PB, TurnoverRate: stk.TurnoverRate,
			Mom: raw[sc.Code].Momentum, SectorMom: sectorMom[stk.Industry],
			Reason: BuildReason(sc.Factors, sc.Score, stk),
		})
	}
	drops := map[string]int{}
	if rest := len(scored) - topN; rest > 0 {
		drops[reasonRankOut] = rest
	}
	tr.emitRaw("topn", "因子排名TopN", len(scored), topN, drops)
	return out
}

// finish 收尾：每级计数写日志；候选为 0 落一条 fail 轨迹（当日日报按降级列出）。
//
// 刻意不发邮件：大盘闸门关闭时"0 候选"是规则的正常输出，天天一封会把告警信道变成噪音。
// 需要立刻知道的异常（数据不新鲜、评审失败、止损触发）由调度器那条 urgent 路径发。
func (s *Screener) finish(ctx context.Context, tradeDate string, rep *Report) error {
	for _, st := range rep.Stages {
		observability.S().Infow("选股漏斗", "date", tradeDate, "stage", st.Stage, "slug", st.Slug,
			"name", st.Name, "in", st.In, "out", st.Out, "drops", formatDrops(st.Drops))
	}
	if !rep.Empty {
		return nil
	}
	summary := funnelSummary(rep.Stages)
	observability.S().Warnw("选股候选为 0", "date", tradeDate, "funnel", summary, "notes", rep.Notes)
	return s.raiseEmptyAlert(ctx, tradeDate, rep, summary)
}

// raiseEmptyAlert 候选为 0 时落一条 alert:SCREEN_EMPTY 轨迹（TraceFail）。
func (s *Screener) raiseEmptyAlert(ctx context.Context, tradeDate string, rep *Report, summary string) error {
	detail := fmt.Sprintf("%s 选股候选 0 条（打分样本 %d 只）。漏斗：%s。板块前三：%s。请人工介入。",
		tradeDate, rep.ScoredTotal, summary, strings.Join(topSectorNames(rep.Sectors, 3), "、"))
	trace := model.RunTrace{
		TradeDate: tradeDate, Subject: model.TraceAlert(AlertCodeScreenEmpty),
		Outcome: model.TraceFail, Detail: detail, At: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.st.TraceRepo().Write(ctx, trace); err != nil {
		return fmt.Errorf("落 SCREEN_EMPTY 轨迹失败：%w", err)
	}
	return nil
}

// load 读取因子窗口内的全部截面数据；窗口有日线缺口时直接报错（不拿旧日期凑数）。
func (s *Screener) load(ctx context.Context, tradeDate string) (*inputs, error) {
	dates, err := s.st.ScreenRepo().WindowDates(ctx, tradeDate, momentumBars)
	if err != nil {
		return nil, err
	}
	if len(dates) < momentumBars {
		return nil, fmt.Errorf("交易日历只有 %d 个交易日（选股需要 %d 个），请先运行 calendar 任务", len(dates), momentumBars)
	}
	gaps, err := s.st.ScreenRepo().WindowBarGaps(ctx, dates)
	if err != nil {
		return nil, err
	}
	if len(gaps) > 0 {
		return nil, fmt.Errorf("日线窗口缺口 %d 个交易日（%s…），请先补跑 daily 任务", len(gaps), strings.Join(gaps[:min(3, len(gaps))], ","))
	}
	series, err := s.st.ScreenRepo().BarCloseSeries(ctx, dates)
	if err != nil {
		return nil, err
	}
	closes, vols, raws := groupSeries(series)
	stocks, err := s.st.ScreenRepo().LiveStocks(ctx)
	if err != nil {
		return nil, err
	}
	susp, err := s.st.MarketRepo().SuspendedCodes(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	suspended := make(map[string]bool, len(susp))
	for _, code := range susp {
		suspended[code] = true
	}
	return &inputs{dates: dates, stocks: stocks, closes: closes, vols: vols,
		raws: raws, suspended: suspended}, nil
}

// groupSeries 将窗口日线点按代码分组为升序的前复权收盘、成交量（手）与未复权收盘（分）。
func groupSeries(series []store.ClosePoint) (closes, vols, raws map[string][]float64) {
	n := max(len(series)/momentumBars, 1)
	closes = make(map[string][]float64, n)
	vols = make(map[string][]float64, n)
	raws = make(map[string][]float64, n)
	for _, p := range series {
		if p.Close > 0 && p.TradeDate != "" {
			closes[p.TsCode] = append(closes[p.TsCode], p.Close)
			vols[p.TsCode] = append(vols[p.TsCode], p.VolLot)
			raws[p.TsCode] = append(raws[p.TsCode], p.RawClose)
		}
	}
	return closes, vols, raws
}

// rankSectors 板块强弱排名：成员流通市值加权的区间涨幅，降序。
//
// 等权排名会把十几个成员的小行业顶到榜首（实测坑），故要求成员数与可算动量数
// 双下限，并按 CircMvW 加权；加权与门槛不满足的行业直接不参与排名（不补位）。
func rankSectors(in *inputs, cfg FilterConfig) []model.SectorStat {
	type acc struct {
		members, scorable int
		wsum, wtot        float64
	}
	byInd := make(map[string]*acc)
	for _, stk := range in.stocks {
		if stk.Industry == "" {
			continue
		}
		a := byInd[stk.Industry]
		if a == nil {
			a = &acc{}
			byInd[stk.Industry] = a
		}
		a.members++
		cs := in.closes[stk.TsCode]
		if len(cs) < momentumBars || cs[0] <= 0 {
			continue
		}
		w := stk.CircMvW
		if w <= 0 {
			continue
		}
		a.scorable++
		a.wsum += (cs[len(cs)-1]/cs[0] - 1) * w
		a.wtot += w
	}
	out := make([]model.SectorStat, 0, len(byInd))
	for ind, a := range byInd {
		if a.members < cfg.MinSectorMembers || a.scorable < minSectorDataMembers || a.wtot <= 0 {
			continue
		}
		out = append(out, model.SectorStat{Industry: ind, Members: a.members, Scorable: a.scorable, WMom: a.wsum / a.wtot})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].WMom != out[j].WMom {
			return out[i].WMom > out[j].WMom
		}
		return out[i].Industry < out[j].Industry
	})
	return out
}

// hotSectors 取前 K 个强势板块并就地标记 Retained，返回归属集合。
func hotSectors(sectors []model.SectorStat, k int) map[string]bool {
	hot := make(map[string]bool, k)
	for i := range sectors {
		if i >= k {
			break
		}
		sectors[i].Retained = true
		hot[sectors[i].Industry] = true
	}
	return hot
}

// topSectorNames 前 n 个板块名（"银行+5.2%/医药+3.1%"样式，供日志与告警）。
func topSectorNames(sectors []model.SectorStat, n int) []string {
	if n > len(sectors) {
		n = len(sectors)
	}
	out := make([]string, 0, n)
	for _, s := range sectors[:n] {
		out = append(out, fmt.Sprintf("%s%+.1f%%", s.Industry, s.WMom*100))
	}
	return out
}

// scorePool 对给定股票集合做四因子打分（不排序）。
func (s *Screener) scorePool(pool []model.StockBasic, in *inputs) ([]Scored, map[string]RawMetrics) {
	codes := make([]string, 0, len(pool))
	raw := make(map[string]RawMetrics, len(pool))
	pePB := make(map[string][2]float64, len(pool))
	for _, stk := range pool {
		codes = append(codes, stk.TsCode)
		raw[stk.TsCode] = ComputeRaw(in.closes[stk.TsCode], stk.TurnoverRate, momentumBars)
		pePB[stk.TsCode] = [2]float64{stk.PETtm, stk.PB}
	}
	factorByCode := buildFactorScores(codes, raw, pePB)
	scored := make([]Scored, 0, len(codes))
	for _, c := range codes {
		fs := factorByCode[c]
		scored = append(scored, Scored{Code: c, Factors: fs, Score: Composite(fs, s.w)})
	}
	return scored, raw
}

// stockOf 在池中按代码取基础信息（候选组装时取行业归属）。
func stockOf(pool []model.StockBasic, code string) model.StockBasic {
	for _, stk := range pool {
		if stk.TsCode == code {
			return stk
		}
	}
	return model.StockBasic{}
}

// tracer 漏斗逐级计数器（结果只进 Report，由 finish 写日志）。
type tracer struct {
	rep   *Report
	drops map[string]int
}

func (t *tracer) drop(why string) {
	if t.drops == nil {
		t.drops = map[string]int{}
	}
	t.drops[why]++
}

// emit 结束一级：写入进出计数并清零淘汰原因。
func (t *tracer) emit(slug, name string, passedIn int, out []model.StockBasic) {
	t.emitRaw(slug, name, passedIn, len(out), t.drops)
	t.drops = nil
}

func (t *tracer) emitRaw(slug, name string, in, out int, drops map[string]int) {
	t.rep.Stages = append(t.rep.Stages, StageStat{
		Stage: len(t.rep.Stages) + 1, Slug: slug, Name: name, In: in, Out: out, Drops: drops,
	})
}

// gate 整级清零的一级（大盘门槛）：全部成员按同一原因淘汰。
func (t *tracer) gate(slug, name string, pool []model.StockBasic, why string) {
	t.emitRaw(slug, name, len(pool), 0, map[string]int{why: len(pool)})
}

// funnelSummary 漏斗的可读摘要（"基础资格 5554→4213；板块强弱TopK 4213→1180 …"）。
func funnelSummary(stages []StageStat) string {
	parts := make([]string, 0, len(stages))
	for _, st := range stages {
		parts = append(parts, fmt.Sprintf("%s %d→%d", st.Name, st.In, st.Out))
	}
	return strings.Join(parts, "；")
}

// formatDrops 淘汰原因分布（日志字段）。
func formatDrops(drops map[string]int) string {
	if len(drops) == 0 {
		return ""
	}
	keys := make([]string, 0, len(drops))
	for k := range drops {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, drops[k]))
	}
	return strings.Join(parts, ",")
}
