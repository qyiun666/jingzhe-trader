package screener

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// AlertCodeScreenEmpty 候选为 0 的 urgent 告警码（附录 B）。
const AlertCodeScreenEmpty = "SCREEN_EMPTY"

// momentumBars 动量/低波因子所需最少日线根数（约一个月）。
const momentumBars = 20

// Screener 选股器：粗筛 → 五因子打分 → TopN → 落库（含漏斗与降级观察名单）。
type Screener struct {
	st  *store.Store
	cfg FilterConfig
	w   FactorWeights
}

// New 构造选股器。cfg 为零值时使用默认参数。
func New(st *store.Store, cfg FilterConfig) *Screener {
	if cfg.TopN <= 0 {
		cfg = DefaultFilterConfig()
	}
	return &Screener{st: st, cfg: cfg, w: DefaultWeights()}
}

// Report 一次选股运行的产出（供 CLI 打印与任务记录）。
type Report struct {
	TradeDate   string
	Candidates  []model.ScreenResult // TopN 候选（已落库）
	FunnelRows  []store.FunnelRow    // 漏斗快照（已落库）
	WatchRows   []store.WatchRow     // 降级观察名单（仅候选为 0 时非空，已落库）
	ScoredTotal int                  // 进入因子打分的股票数
	Empty       bool                 // 候选是否为 0
}

// Run 执行选股并落库（screen_result / screen_funnel / 必要时 screen_watchlist + SCREEN_EMPTY 告警）。
// 数据全部读自 SQLite（Tushare 已由 data 任务入库），本函数不触网。
func (s *Screener) Run(ctx context.Context, tradeDate string) (*Report, error) {
	rep := &Report{TradeDate: tradeDate}

	// ---------- 数据装载 ----------
	bars, err := s.st.ScreenRepo().RecentTradeDates(ctx, tradeDate, momentumBars)
	if err != nil {
		return nil, err
	}
	if len(bars) == 0 {
		return nil, fmt.Errorf("选股失败: daily_bar 无 %s 及之前的任何数据（请先运行 data 任务）", tradeDate)
	}
	fromDate := bars[len(bars)-1]
	series, err := s.st.ScreenRepo().BarCloseSeries(ctx, fromDate)
	if err != nil {
		return nil, err
	}
	closesByCode := groupCloses(series)
	basics, err := s.st.ScreenRepo().BasicAt(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	basicByCode := make(map[string]model.DailyBasic, len(basics))
	for _, b := range basics {
		basicByCode[b.TsCode] = b
	}
	stocks, err := s.st.ScreenRepo().LiveStocks(ctx)
	if err != nil {
		return nil, err
	}
	suspended, err := s.st.ScreenRepo().SuspendedMap(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	finas, err := s.st.ScreenRepo().FinaLatestAsOf(ctx, tradeDate)
	if err != nil {
		return nil, err
	}

	// ---------- 漏斗逐级筛选 ----------
	type dropStats map[string]int
	newDrops := func() dropStats { return dropStats{} }
	stages := make([]store.FunnelRow, 0, 4)

	survivors := make([]model.StockBasic, 0, len(stocks))
	drops := newDrops()
	passedIn := len(stocks)
	for _, stk := range stocks {
		ok, why := basicEligible(stk, listDaysBetween(stk.ListDate, tradeDate), s.cfg.MinListDays, suspended[stk.TsCode])
		if ok {
			survivors = append(survivors, stk)
		} else {
			drops[why]++
		}
	}
	stages = append(stages, sages(passedIn, len(survivors), drops, "基础资格(ST/新股/停牌)")...)
	stage1 := append([]model.StockBasic(nil), survivors...) // 一级存量快照（降级观察名单回退用）

	// 流动性
	passedIn = len(survivors)
	liqOK := make([]model.StockBasic, 0, len(survivors))
	drops = newDrops()
	for _, stk := range survivors {
		b, ok := basicByCode[stk.TsCode]
		if !ok {
			drops[reasonNoBasic]++
			continue
		}
		if ok, why := liquidityStage(b, s.cfg); ok {
			liqOK = append(liqOK, stk)
		} else {
			drops[why]++
		}
	}
	survivors = liqOK
	stages = append(stages, sages(passedIn, len(survivors), drops, "流动性(市值/换手)")...)

	// 估值与价格
	passedIn = len(survivors)
	valOK := make([]model.StockBasic, 0, len(survivors))
	drops = newDrops()
	for _, stk := range survivors {
		b := basicByCode[stk.TsCode]
		if ok, why := valuationStage(b, s.cfg); ok {
			valOK = append(valOK, stk)
		} else {
			drops[why]++
		}
	}
	survivors = valOK
	stages = append(stages, sages(passedIn, len(survivors), drops, "估值价格(PE/PB/区间)")...)

	rep.ScoredTotal = len(survivors)

	// 逐级存量快照：候选为 0 时观察名单回退到最后一级仍有存量的集合
	poolStage1 := append([]model.StockBasic(nil), stage1...)
	poolStage2 := append([]model.StockBasic(nil), liqOK...)
	poolStage3 := append([]model.StockBasic(nil), valOK...)

	// ---------- 因子打分 ----------
	allScored, raw := s.scorePool(survivors, closesByCode, basicByCode, finas)
	sort.SliceStable(allScored, func(i, j int) bool {
		if allScored[i].Score != allScored[j].Score {
			return allScored[i].Score > allScored[j].Score
		}
		return allScored[i].Code < allScored[j].Code
	})

	// ---------- TopN ----------
	topN := s.cfg.TopN
	if topN > len(allScored) {
		topN = len(allScored)
	}
	thJSON, _ := json.Marshal(s.cfg) // 序列化失败时 thresholds 留空，不阻断

	passedIn = len(allScored)
	drops = newDrops()
	if rest := len(allScored) - topN; rest > 0 {
		drops[reasonRankOut] = rest
	}
	stages = append(stages, sages(passedIn, len(allScored), drops, "因子排名TopN")...)
	for i := range stages {
		stages[i].TradeDate = tradeDate
		stages[i].Stage = i + 1 // 漏斗级序号从 1 开始
		stages[i].Thresholds = string(thJSON)
	}

	rep.FunnelRows = stages

	// ---------- 组装结果 ----------
	nameMap, err := s.st.ScreenRepo().StockNameMap(ctx)
	if err != nil {
		nameMap = map[string]string{} // 名称缺失不阻断选股，reason 中省略名称
	}

	if topN == 0 {
		rep.Empty = true
		// 降级观察名单：最后一级（因子打分）无存量时，回退到最后一级仍有存量的漏斗级，
		// 保证 SCREEN_EMPTY 时人工介入始终有 Top20 可看（验收 #3）。
		watchScored, watchStage := allScored, ""
		if len(watchScored) == 0 {
			fallback := []struct {
				pool []model.StockBasic
				name string
			}{{poolStage3, "估值价格(PE/PB/区间)"}, {poolStage2, "流动性(市值/换手)"}, {poolStage1, "基础资格(ST/新股/停牌)"}}
			for _, fb := range fallback {
				if len(fb.pool) > 0 {
					watchScored, _ = s.scorePool(fb.pool, closesByCode, basicByCode, finas)
					watchStage = fb.name
					break
				}
			}
		}
		sort.SliceStable(watchScored, func(i, j int) bool {
			if watchScored[i].Score != watchScored[j].Score {
				return watchScored[i].Score > watchScored[j].Score
			}
			return watchScored[i].Code < watchScored[j].Code
		})
		rep.WatchRows = s.buildWatchlist(tradeDate, watchScored, watchStage, watchBudget)
		if err := s.st.ScreenRepo().ReplaceScreenResults(ctx, tradeDate, nil); err != nil {
			return nil, err
		}
		if err := s.st.ScreenRepo().ReplaceWatchlist(ctx, tradeDate, rep.WatchRows); err != nil {
			return nil, err
		}
		if err := s.st.ScreenRepo().ReplaceFunnel(ctx, tradeDate, stages); err != nil {
			return nil, err
		}
		if err := s.raiseEmptyAlert(ctx, tradeDate, rep); err != nil {
			return nil, err
		}
		return rep, nil
	}

	results := make([]model.ScreenResult, 0, topN)
	for rank, sc := range allScored[:topN] {
		b := basicByCode[sc.Code]
		rm := raw[sc.Code]
		reason := BuildReason(sc.Factors, sc.Score, b, rm.Quality, rm.HasQual)
		if nm, ok := nameMap[sc.Code]; ok && nm != "" {
			reason = nm + "：" + reason
		}
		results = append(results, model.ScreenResult{
			TradeDate:    tradeDate,
			TsCode:       sc.Code,
			Rank:         rank + 1,
			Score:        sc.Score,
			Factors:      sc.Factors,
			F_Momentum:   sc.Factors.Momentum,
			F_Quality:    sc.Factors.Quality,
			F_Value:      sc.Factors.Value,
			F_LowVol:     sc.Factors.LowVol,
			F_Liquidity:  sc.Factors.Liquidity,
			Close:        b.Close,
			CircMvW:      b.CircMvW,
			PETtm:        b.PETtm,
			PB:           b.PB,
			TurnoverRate: b.TurnoverRate,
			Reason:       reason,
		})
	}
	rep.Candidates = results

	if err := s.st.ScreenRepo().ReplaceScreenResults(ctx, tradeDate, results); err != nil {
		return nil, err
	}
	if err := s.st.ScreenRepo().ReplaceFunnel(ctx, tradeDate, stages); err != nil {
		return nil, err
	}
	return rep, nil
}

// watchBudget 候选为 0 时写入观察名单的容量。
const watchBudget = 20

// scorePool 对给定股票集合做五因子打分（不排序），供 TopN 与降级观察名单共用。
func (s *Screener) scorePool(pool []model.StockBasic, closesByCode map[string][]float64,
	basicByCode map[string]model.DailyBasic, finas map[string]model.FinaIndicator) ([]Scored, map[string]RawMetrics) {
	codes := make([]string, 0, len(pool))
	raw := make(map[string]RawMetrics, len(pool))
	pePB := make(map[string][2]float64, len(pool))
	for _, stk := range pool {
		codes = append(codes, stk.TsCode)
		b := basicByCode[stk.TsCode]
		cs := closesByCode[stk.TsCode]
		rm := ComputeRaw(cs, b.TurnoverRate, 0, false, momentumBars)
		if fi, ok := finas[stk.TsCode]; ok {
			rm.Quality = fi.ROE
			rm.HasQual = fi.ROE != 0
		}
		pePB[stk.TsCode] = [2]float64{b.PETtm, b.PB}
		raw[stk.TsCode] = rm
	}
	factorByCode := buildFactorScores(codes, raw, pePB)
	scored := make([]Scored, 0, len(codes))
	for _, c := range codes {
		fs := factorByCode[c]
		scored = append(scored, Scored{Code: c, Factors: fs, Score: Composite(fs, s.w)})
	}
	return scored, raw
}

// buildWatchlist 降级观察名单：取给定打分集合的 TopN，并注明未过原因，便于人工介入。
// watchStage 非空时表示回退到的漏斗级（该级之后被阈值拦下）。
func (s *Screener) buildWatchlist(tradeDate string, scored []Scored, watchStage string, budget int) []store.WatchRow {
	if len(scored) == 0 {
		return nil
	}
	if budget > len(scored) {
		budget = len(scored)
	}
	rows := make([]store.WatchRow, 0, budget)
	for i := 0; i < budget; i++ {
		sc := scored[i]
		why := "因子分不足未进TopN"
		if watchStage != "" {
			why = "被[" + watchStage + "]级阈值拦截"
		} else if sc.Score <= 0 {
			why = "因子分过低"
		}
		rows = append(rows, store.WatchRow{
			TradeDate: tradeDate,
			TsCode:    sc.Code,
			Rank:      i + 1,
			Score:     sc.Score,
			Reason:    fmt.Sprintf("候选为0降级观察（未过原因：%s，得分 %.1f）", why, sc.Score),
		})
	}
	return rows
}

// raiseEmptyAlert 候选为 0 时落 SCREEN_EMPTY urgent 告警（含漏斗摘要，诊断不黑盒）。
func (s *Screener) raiseEmptyAlert(ctx context.Context, tradeDate string, rep *Report) error {
	content := fmt.Sprintf("%s 选股候选 0 条（打分样本 %d 只）。漏斗：%s。观察名单已降级写入 %d 只，请人工介入。",
		tradeDate, rep.ScoredTotal, funnelSummary(rep.FunnelRows), len(rep.WatchRows))
	alert := model.AgentAlert{
		TradeDate: tradeDate,
		Source:    "screener",
		Level:     model.AlertUrgent,
		Code:      AlertCodeScreenEmpty,
		Title:     "选股候选为空",
		Content:   content,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.st.OpsRepo().RaiseAlert(ctx, alert); err != nil {
		return fmt.Errorf("落 SCREEN_EMPTY 告警失败: %w", err)
	}
	return nil
}

// funnelSummary 生成漏斗一行的可读摘要（"基础资格 5554→4300；流动性 …"）。
func funnelSummary(rows []store.FunnelRow) string {
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += "；"
		}
		out += fmt.Sprintf("%s %d→%d", r.StageName, r.PassedIn, r.PassedOut)
	}
	return out
}

// sages 组装单级漏斗行（stage 序号由切片长度推导，调用方按序追加）。
func sages(passedIn, passedOut int, drops map[string]int, name string) []store.FunnelRow {
	total := 0
	keys := make([]string, 0, len(drops))
	for k, v := range drops {
		keys = append(keys, k)
		total += v
	}
	sort.Strings(keys) // 稳定输出
	dj, _ := json.Marshal(drops)
	return []store.FunnelRow{{
		Stage:       0, // 由调用方追加后统一编号
		StageName:   name,
		PassedIn:    passedIn,
		PassedOut:   passedOut,
		Dropped:     passedIn - passedOut,
		DropReasons: string(dj),
	}}
}

// groupCloses 将日线序列点按代码分组为升序前复权收盘切片。
func groupCloses(series []store.ClosePoint) map[string][]float64 {
	m := make(map[string][]float64)
	for _, p := range series {
		if p.Close > 0 && !math.IsNaN(p.Close) {
			m[p.TsCode] = append(m[p.TsCode], p.Close)
		}
	}
	return m
}
