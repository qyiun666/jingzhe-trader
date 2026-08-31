package screener

import (
	"fmt"
	"math"
	"sort"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
	"jingzhe-trader/pkg/logger"
)

// ScreenResult 选股结果 (定义在 store 包, 此处别名, 避免循环依赖)
type ScreenResult = store.ScreenResult

// Screener 自动选股器
// 每日收盘后从全市场筛选候选股票, 补充到策略股票池
type Screener struct {
	ts         *tushare.Client
	stockRepo  *store.StockRepo
	barRepo    *store.BarRepo
	basicRepo  *store.BasicRepo
	screenRepo *store.ScreenRepo
	cfg        config.ScreenerConfig
	capital    CapitalSource
}

// Deps 选股器依赖, 由组合根一次性装配
type Deps struct {
	TS         *tushare.Client
	StockRepo  *store.StockRepo
	BarRepo    *store.BarRepo
	BasicRepo  *store.BasicRepo
	ScreenRepo *store.ScreenRepo
	Cfg        config.ScreenerConfig
	Capital    CapitalSource // 可 nil: 无资金视图时退回配置里的静态价格区间
}

// New 创建选股器
func New(d Deps) *Screener {
	return &Screener{
		ts:         d.TS,
		stockRepo:  d.StockRepo,
		barRepo:    d.BarRepo,
		basicRepo:  d.BasicRepo,
		screenRepo: d.ScreenRepo,
		cfg:        d.Cfg,
		capital:    d.Capital,
	}
}

// Screen 执行全市场选股
// 1. 拉取全市场 daily_basic + daily (不经过 filter_mode)
// 2. 计算近5日收盘均线与动量 (本地 daily_bar 优先, 缺失走 API)
// 3. 逐只过滤: 配置区间 ∩ 资金反推价格带 + 方向门槛 (站上MA5 且 5日动量非负, 数据不足即淘汰)
// 4. 评分排序取 TopN, 同行业最多3只 (行业分散)
// 5. 同步候选股票历史K线与当日基本面 (策略均线/辩论基本面计算需要)
// 6. 结果落库
func (s *Screener) Screen(date string) ([]ScreenResult, error) {
	if !s.cfg.Enabled {
		logger.L().Info("选股器未启用, 跳过")
		return nil, nil
	}

	logger.L().Infow("开始全市场选股", "date", date, "max_candidates", s.cfg.MaxCandidates)

	// 1. 拉取全市场基本面
	basics, err := s.ts.DailyBasic(date)
	if err != nil {
		return nil, fmt.Errorf("拉取全市场基本面失败: %w", err)
	}
	logger.L().Infow("全市场基本面拉取完成", "count", len(basics))

	// 2. 拉取全市场日线 (用于涨跌幅)
	bars, err := s.ts.Daily(date)
	if err != nil {
		return nil, fmt.Errorf("拉取全市场日线失败: %w", err)
	}
	barMap := make(map[string]model.Bar, len(bars))
	for _, b := range bars {
		barMap[b.TsCode] = b
	}

	// 2.5 拉取近5个交易日收盘价 (用于趋势和动量计算)
	recentCloses := s.fetchRecentCloses(date)
	logger.L().Infow("近5日收盘价获取完成", "stocks", len(recentCloses))

	// 3. 获取股票基本信息 (ST/上市日期)
	stocks, err := s.stockRepo.GetAll()
	if err != nil {
		return nil, fmt.Errorf("获取股票列表失败: %w", err)
	}
	stockMap := make(map[string]model.Stock, len(stocks))
	for _, st := range stocks {
		stockMap[st.TsCode] = st
	}

	// 4. 构建配置池排除集合 (已在配置池中的不需要重复选)
	excludeCodes := make(map[string]bool)
	for _, code := range s.cfg.ExcludeCodes {
		excludeCodes[code] = true
	}

	// 4.5 价格区间与资金反推的价格带取交集
	// 资金决定可选域: 买不起 1 手的高价股、凑不满最小单笔金额的低价股都必然被风控裁掉,
	// 选进候选池只会挤掉真正可执行的标的
	money := s.capitalSnapshot()
	minPrice, maxPrice := priceBandOf(s.cfg, money)
	logger.L().Infow("选股价格区间", "min_price", minPrice, "max_price", maxPrice,
		"total_asset", money.TotalAsset, "cash", money.Cash)

	// 5. 多维度筛选 (逐只过滤 + 按原因计数, 空候选日必须能说清是被哪条规则清空)
	env := filterEnv{
		date:     date,
		exclude:  excludeCodes,
		stocks:   stockMap,
		bars:     barMap,
		recent:   recentCloses,
		minPrice: minPrice,
		maxPrice: maxPrice,
	}
	candidates, dropped := s.filterCandidates(basics, env)
	logger.L().Infow("初筛完成", "count", len(candidates), "dropped", dropped)

	// 6. 按评分排序取 TopN, 同行业最多3只 (避免集中)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	industries := make(map[string]string, len(stockMap))
	for code, st := range stockMap {
		industries[code] = st.Industry
	}
	candidates = diversifyByIndustry(candidates, industries, s.cfg.MaxCandidates, 3)

	// 7. 同步候选股票历史K线 (策略需要均线数据, 约半年)
	if len(candidates) > 0 {
		s.syncHistoryBars(candidates, date)
		s.saveCandidateBasics(candidates, basics)
	}

	// 8. 结果落库
	if err := s.screenRepo.SaveResults(date, candidates); err != nil {
		logger.L().Warnw("选股结果落库失败", "err", err)
	}

	// 9. 同步当日日线到 daily_bar (策略 OnBar 需要当日行情)
	s.syncTodayBars(candidates, barMap)

	logger.L().Infow("选股完成", "date", date, "candidates", len(candidates))
	for i, c := range candidates {
		logger.L().Infof("  #%d %s %s  收盘%.2f 涨跌%.2f%% 换手%.2f%% PE_TTM=%.1f PB=%.2f 评分%.2f",
			i+1, c.TsCode, c.Name, c.Close, c.PctChg, c.TurnoverRate, c.PE_TTM, c.PB, c.Score)
	}

	return candidates, nil
}

// filterEnv 初筛一次运行内共享的只读上下文
type filterEnv struct {
	date     string                 // 选股日期 YYYYMMDD
	exclude  map[string]bool        // 配置池已有代码
	stocks   map[string]model.Stock // ts_code → 基本信息
	bars     map[string]model.Bar   // ts_code → 当日日线
	recent   map[string][]float64   // ts_code → 近 N 日收盘 (由近及远)
	minPrice float64                // 价格带下限 (含资金反推)
	maxPrice float64                // 价格带上限 (含资金反推)
}

// filterCandidates 对全市场基本面逐只跑过滤条件, 返回候选与按规则计数的淘汰数
func (s *Screener) filterCandidates(basics []model.DailyBasic, e filterEnv) ([]ScreenResult, map[string]int) {
	dropped := make(map[string]int)
	candidates := make([]ScreenResult, 0, len(basics))
	for _, basic := range basics {
		res, rule := s.filterOne(basic, e)
		if rule != "" {
			dropped[rule]++
			continue
		}
		candidates = append(candidates, res)
	}
	return candidates, dropped
}

// filterOne 单只股票的过滤 + 评分
// 通过时 rule 为空串; 被淘汰时 rule 为淘汰原因标签 (供计数)
func (s *Screener) filterOne(basic model.DailyBasic, e filterEnv) (ScreenResult, string) {
	stock := e.stocks[basic.TsCode]
	switch {
	case e.exclude[basic.TsCode]:
		return ScreenResult{}, "已在配置池"
	case s.cfg.ExcludeST && stock.IsST:
		return ScreenResult{}, "ST股"
	case stock.ListStatus != "" && stock.ListStatus != "L":
		return ScreenResult{}, "非上市状态"
	case !s.listedLongEnough(stock):
		return ScreenResult{}, "上市不足"
	case model.IsLimitUp(basic.LimitStatus) || model.IsLimitDown(basic.LimitStatus):
		return ScreenResult{}, "当日涨跌停"
	case e.minPrice > 0 && basic.Close < e.minPrice:
		return ScreenResult{}, "低于资金可买价格带下限"
	case e.maxPrice > 0 && basic.Close > e.maxPrice:
		return ScreenResult{}, "高于资金可买价格带上限"
	case s.cfg.MinTurnoverRate > 0 && basic.TurnoverRate < s.cfg.MinTurnoverRate:
		return ScreenResult{}, "换手率不足"
	case s.cfg.MaxPE > 0 && (basic.PE_TTM <= 0 || basic.PE_TTM > s.cfg.MaxPE):
		return ScreenResult{}, "PE区间外"
	case s.cfg.MaxPB > 0 && (basic.PB <= 0 || basic.PB > s.cfg.MaxPB):
		return ScreenResult{}, "PB区间外"
	case s.cfg.MinCircMV > 0 && basic.CircMV < s.cfg.MinCircMV:
		return ScreenResult{}, "流通市值过小"
	case s.cfg.MaxCircMV > 0 && basic.CircMV > s.cfg.MaxCircMV:
		return ScreenResult{}, "流通市值过大"
	}

	// 方向门槛: 买入候选必须处于上行方向
	// 样本不足时按"方向未知"淘汰 —— 原实现用 `ma5 > 0 &&` 短路, 缺数据等于放行,
	// 会把趋势过滤变成只在有数据的日子生效的摆设
	t := calcTrend(e.recent[basic.TsCode], basic.Close)
	if t.MA5 <= 0 {
		return ScreenResult{}, "趋势数据不足"
	}
	if basic.Close < t.MA5 {
		return ScreenResult{}, "收盘低于MA5"
	}
	if t.Momentum < 0 {
		return ScreenResult{}, "5日动量为负"
	}

	pctChg := 0.0
	if bar, ok := e.bars[basic.TsCode]; ok {
		pctChg = bar.PctChg
	}
	name := stock.Name
	if name == "" {
		name = basic.TsCode
	}
	return ScreenResult{
		TsCode:       basic.TsCode,
		Name:         name,
		TradeDate:    e.date,
		Close:        basic.Close,
		PctChg:       pctChg,
		TurnoverRate: basic.TurnoverRate,
		VolumeRatio:  basic.VolumeRatio,
		PE:           basic.PE,
		PE_TTM:       basic.PE_TTM,
		PB:           basic.PB,
		CircMV:       basic.CircMV,
		Score:        s.calcScore(basic, pctChg, t),
		Reason:       s.buildReason(basic, pctChg, t),
	}, ""
}

// listedLongEnough 上市是否满 min_list_days (无上市日期信息时不因此淘汰)
func (s *Screener) listedLongEnough(stock model.Stock) bool {
	if s.cfg.MinListDays <= 0 || stock.ListDate == "" {
		return true
	}
	listTime, err := time.Parse("20060102", stock.ListDate)
	if err != nil {
		return true
	}
	return int(time.Since(listTime).Hours()/24) >= s.cfg.MinListDays
}

// capitalSnapshot 取当前资金快照 (未注入资金源时返回零值)
func (s *Screener) capitalSnapshot() Capital {
	if s.capital == nil {
		return Capital{}
	}
	return s.capital.Capital()
}

// priceBandOf 配置价格区间与资金反推价格带取交集 (0 表示该侧不设限)
func priceBandOf(cfg config.ScreenerConfig, money Capital) (minPrice, maxPrice float64) {
	lo, hi := money.PriceBand()
	pick := func(a, b float64, widest bool) float64 {
		if a <= 0 {
			return b
		}
		if b <= 0 {
			return a
		}
		if widest {
			return math.Max(a, b) // 下限取更严的一侧
		}
		return math.Min(a, b)
	}
	return pick(cfg.MinPrice, lo, true), pick(cfg.MaxPrice, hi, false)
}

// saveCandidateBasics 把候选股票当日基本面写回 daily_basic
// dataloader 的 filter_mode 会把 daily_basic 裁到配置股票池, 新选出的候选因此常年无基本面行;
// 辩论的基本面分析师拿到全 0 的 PE/PB 不会报错, 只会据此判定"估值异常"并给出偏空结论
func (s *Screener) saveCandidateBasics(candidates []ScreenResult, basics []model.DailyBasic) {
	if s.basicRepo == nil || len(candidates) == 0 {
		return
	}
	want := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		want[c.TsCode] = true
	}
	var rows []model.DailyBasic
	for _, b := range basics {
		if want[b.TsCode] {
			rows = append(rows, b)
		}
	}
	if err := s.basicRepo.BatchInsert(rows); err != nil {
		logger.L().Warnw("候选股票基本面落库失败", "err", err, "rows", len(rows))
	}
}

// syncHistoryBars 同步候选股票历史K线 (约6个月, 策略均线计算需要)
func (s *Screener) syncHistoryBars(candidates []ScreenResult, date string) {
	if s.ts == nil {
		return
	}
	// 计算6个月前的日期
	endDate := date
	t, err := time.Parse("20060102", date)
	if err != nil {
		return
	}
	startDate := t.AddDate(0, -6, 0).Format("20060102")

	synced := 0
	for _, c := range candidates {
		// 检查是否已有足够历史数据
		existing, err := s.barRepo.GetBars(c.TsCode, startDate, endDate)
		if err == nil && len(existing) >= 60 {
			continue // 已有足够数据, 跳过
		}

		// 拉取历史日线
		bars, err := s.ts.DailyByCode(c.TsCode, startDate, endDate)
		if err != nil {
			logger.L().Warnw("同步历史K线失败", "ts_code", c.TsCode, "err", err)
			continue
		}
		if len(bars) == 0 {
			continue
		}
		if err := s.barRepo.BatchInsert(bars); err != nil {
			logger.L().Warnw("写入历史K线失败", "ts_code", c.TsCode, "err", err)
			continue
		}
		synced++
	}
	if synced > 0 {
		logger.L().Infow("候选股票历史K线同步完成", "synced", synced, "total", len(candidates))
	}
}

// syncTodayBars 同步候选股票当日日线到 daily_bar (策略 OnBar 需要)
func (s *Screener) syncTodayBars(candidates []ScreenResult, barMap map[string]model.Bar) {
	var barsToInsert []model.Bar
	for _, c := range candidates {
		if bar, ok := barMap[c.TsCode]; ok {
			barsToInsert = append(barsToInsert, bar)
		}
	}
	if len(barsToInsert) > 0 {
		if err := s.barRepo.BatchInsert(barsToInsert); err != nil {
			logger.L().Warnw("写入候选股票当日行情失败", "err", err)
		}
	}
}

// diversifyByIndustry 按行业分散选股: 同行业最多 maxPerIndustry 只
// industries 为 ts_code → 行业名称映射 (来自 stock_basic), 缺失回退 "unknown"
// 候选不足或行业高度集中时允许少于 maxN, 不撤销分散约束
func diversifyByIndustry(candidates []ScreenResult, industries map[string]string, maxN, maxPerIndustry int) []ScreenResult {
	industryCount := make(map[string]int)
	result := make([]ScreenResult, 0, maxN)
	for _, c := range candidates {
		if len(result) >= maxN {
			break
		}
		industry := industries[c.TsCode]
		if industry == "" {
			industry = "unknown"
		}
		if industryCount[industry] >= maxPerIndustry {
			continue
		}
		industryCount[industry]++
		result = append(result, c)
	}
	if len(result) < maxN {
		logger.L().Infow("行业分散后未凑满候选数", "selected", len(result), "max", maxN, "total", len(candidates))
	}
	return result
}
