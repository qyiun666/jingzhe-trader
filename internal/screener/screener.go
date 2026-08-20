package screener

import (
	"fmt"
	"sort"
	"strings"
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
	screenRepo *store.ScreenRepo
	cfg        config.ScreenerConfig
}

// New 创建选股器
func New(ts *tushare.Client, stockRepo *store.StockRepo, barRepo *store.BarRepo, screenRepo *store.ScreenRepo, cfg config.ScreenerConfig) *Screener {
	return &Screener{
		ts:         ts,
		stockRepo:  stockRepo,
		barRepo:    barRepo,
		screenRepo: screenRepo,
		cfg:        cfg,
	}
}

// Screen 执行全市场选股
// 1. 拉取全市场 daily_basic + daily (不经过 filter_mode)
// 2. 计算近5日收盘均线与动量 (本地 daily_bar 优先, 缺失走 API)
// 3. 多维度筛选 + 趋势过滤 (收盘价低于MA5的下跌趋势股剔除)
// 4. 评分排序取 TopN, 同行业最多3只 (行业分散)
// 5. 同步候选股票历史K线 (策略均线计算需要)
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

	// 5. 多维度筛选
	var candidates []ScreenResult
	for _, basic := range basics {
		// 排除配置池已有
		if excludeCodes[basic.TsCode] {
			continue
		}

		// 排除ST/退市
		if stock, ok := stockMap[basic.TsCode]; ok {
			if s.cfg.ExcludeST && stock.IsST {
				continue
			}
			if stock.ListStatus != "L" {
				continue
			}
			// 排除新股 (上市不足 min_list_days 天)
			if s.cfg.MinListDays > 0 && stock.ListDate != "" {
				if listTime, err := time.Parse("20060102", stock.ListDate); err == nil {
					days := time.Since(listTime).Hours() / 24
					if int(days) < s.cfg.MinListDays {
						continue
					}
				}
			}
		}

		// 涨跌停过滤 (limit_status: 1涨停 -1跌停 0正常)
		if basic.LimitStatus != 0 {
			continue
		}

		// 价格区间
		if s.cfg.MinPrice > 0 && basic.Close < s.cfg.MinPrice {
			continue
		}
		if s.cfg.MaxPrice > 0 && basic.Close > s.cfg.MaxPrice {
			continue
		}

		// 换手率
		if s.cfg.MinTurnoverRate > 0 && basic.TurnoverRate < s.cfg.MinTurnoverRate {
			continue
		}

		// PE过滤 (排除负PE和过高PE)
		if s.cfg.MaxPE > 0 && (basic.PE_TTM <= 0 || basic.PE_TTM > s.cfg.MaxPE) {
			continue
		}

		// PB过滤
		if s.cfg.MaxPB > 0 && (basic.PB <= 0 || basic.PB > s.cfg.MaxPB) {
			continue
		}

		// 流通市值过滤 (单位: 万元)
		if s.cfg.MinCircMV > 0 && basic.CircMV < s.cfg.MinCircMV {
			continue
		}
		if s.cfg.MaxCircMV > 0 && basic.CircMV > s.cfg.MaxCircMV {
			continue
		}

		// 获取涨跌幅
		pctChg := 0.0
		if bar, ok := barMap[basic.TsCode]; ok {
			pctChg = bar.PctChg
		}

		// 趋势过滤: 近5日均价低于当日收盘 → 下跌趋势, 跳过
		ma5, momentum5d, trendDays := calcTrend(recentCloses[basic.TsCode], basic.Close)
		if ma5 > 0 && basic.Close < ma5 {
			continue // 收盘价低于MA5, 短期下跌趋势, 跳过
		}

		// 评分: 换手率 + 量比 + 估值 + 趋势 + 动量
		score := s.calcScore(basic, pctChg, ma5, momentum5d)

		stockName := basic.TsCode
		if stock, ok := stockMap[basic.TsCode]; ok {
			stockName = stock.Name
		}

		candidates = append(candidates, ScreenResult{
			TsCode:       basic.TsCode,
			Name:         stockName,
			TradeDate:    date,
			Close:        basic.Close,
			PctChg:       pctChg,
			TurnoverRate: basic.TurnoverRate,
			VolumeRatio:  basic.VolumeRatio,
			PE:           basic.PE,
			PE_TTM:       basic.PE_TTM,
			PB:           basic.PB,
			CircMV:       basic.CircMV,
			Score:        score,
			Reason:       s.buildReason(basic, pctChg, ma5, momentum5d, trendDays),
		})
	}

	logger.L().Infow("初筛完成", "count", len(candidates))

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

// calcScore 评分: 综合活跃度、资金关注度、估值、趋势、动量
// 权重: 换手20% + 量比20% + 估值15% + 趋势20% + 动量15% + 当日涨跌10%
// 量纲: 仅用于候选排序比较, 分数会随权重调整变化, 禁止下游按绝对值设阈值
func (s *Screener) calcScore(basic model.DailyBasic, pctChg, ma5, momentum5d float64) float64 {
	// 活跃度: 换手率 (1-10% 最佳, 过高可能是炒作)
	turnoverScore := 0.0
	if basic.TurnoverRate > 0 {
		if basic.TurnoverRate <= 10 {
			turnoverScore = basic.TurnoverRate
		} else {
			turnoverScore = 10 - (basic.TurnoverRate-10)*0.5
		}
	}

	// 资金关注度: 量比 (>1 放量, >5 封顶低分防异常)
	volRatioScore := 0.0
	if basic.VolumeRatio > 0 {
		if basic.VolumeRatio <= 3 {
			volRatioScore = basic.VolumeRatio
		} else if basic.VolumeRatio <= 5 {
			volRatioScore = 3 - (basic.VolumeRatio-3)*0.5
		} else {
			volRatioScore = 1.0 // 异常放量封顶低分
		}
	}

	// 估值吸引力: PE_TTM (10-30 最佳)
	peScore := 0.0
	if basic.PE_TTM > 0 {
		if basic.PE_TTM >= 10 && basic.PE_TTM <= 30 {
			peScore = 3.0
		} else if basic.PE_TTM > 30 && basic.PE_TTM <= 50 {
			peScore = 1.5
		} else if basic.PE_TTM < 10 {
			peScore = 2.0
		}
	}

	// 趋势分: 收盘价高于MA5越多越好 (上限3分)
	trendScore := 0.0
	if ma5 > 0 {
		deviation := (basic.Close - ma5) / ma5 * 100 // 偏离百分比
		if deviation >= 0 && deviation <= 3 {
			trendScore = 2.0 + deviation/3 // 2~3分
		} else if deviation > 3 && deviation <= 8 {
			trendScore = 3.0 - (deviation-3)*0.2 // 3分递减
		} else if deviation > 8 {
			trendScore = 2.0 // 过高也降分
		} else if deviation < 0 {
			trendScore = 1.0 // 低于MA5给低分 (不应到这里, 但防止边界)
		}
	}

	// 动量分: 5日涨幅 (温和上涨最佳, 暴涨递减; 含走平消除 0 分空洞)
	momentumScore := 0.0
	if momentum5d >= 0 && momentum5d <= 5 {
		momentumScore = 3.0 // 含走平
	} else if momentum5d > 5 && momentum5d <= 10 {
		momentumScore = 2.0
	} else if momentum5d > 10 {
		momentumScore = 1.0 // 暴涨可能追高
	} else if momentum5d < 0 && momentum5d >= -3 {
		momentumScore = 1.0 // 小幅回调
	} else if momentum5d < -3 {
		momentumScore = 0.0 // 大跌不给分
	}

	// 当日涨跌: 小涨优于大跌
	chgScore := 0.0
	if pctChg > 0 && pctChg <= 5 {
		chgScore = 2.0
	} else if pctChg > 5 && pctChg <= 9 {
		chgScore = 1.0
	} else if pctChg < -3 {
		chgScore = -1.0
	}

	return turnoverScore*0.20 + volRatioScore*0.20 + peScore*0.15 +
		trendScore*0.20 + momentumScore*0.15 + chgScore*0.10
}

// fetchRecentCloses 获取近5个交易日的收盘价 (用于趋势和动量计算)
// 每日期优先读本地 daily_bar, 本地无数据时走 Tushare API; 最多回溯 10 个自然日 (覆盖长假)
func (s *Screener) fetchRecentCloses(today string) map[string][]float64 {
	t, err := time.Parse("20060102", today)
	if err != nil {
		logger.L().Warnw("选股日期解析失败", "date", today, "err", err)
		return nil
	}

	result := make(map[string][]float64)
	collected := 0
	for i := 1; i <= 15 && collected < 5; i++ { // 15 个自然日覆盖春节/国庆长假
		date := t.AddDate(0, 0, -i)
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			continue
		}
		dateStr := date.Format("20060102")
		bars := s.closesOfDate(dateStr)
		if len(bars) == 0 {
			continue // 非交易日或该日无数据
		}
		for _, bar := range bars {
			result[bar.TsCode] = append(result[bar.TsCode], bar.Close)
		}
		collected++
	}
	return result
}

// closesOfDate 获取某交易日全市场日线: 优先本地 daily_bar, 缺失时走 Tushare API
func (s *Screener) closesOfDate(dateStr string) []model.Bar {
	if s.barRepo != nil {
		if bars, err := s.barRepo.GetBarsByDate(dateStr); err == nil && len(bars) > 0 {
			return bars
		} else if err != nil {
			logger.L().Warnw("查询本地日线失败, 回退 API", "date", dateStr, "err", err)
		}
	}
	bars, err := s.ts.Daily(dateStr)
	if err != nil {
		logger.L().Warnw("拉取历史收盘价失败", "date", dateStr, "err", err)
		return nil
	}
	return bars
}

// calcTrend 根据近N日收盘价计算均线和N日动量, days 为实际收集到的天数
// recentCloses 按日期从近到远排列 (index 0 为最近一天)
func calcTrend(recentCloses []float64, todayClose float64) (ma5, momentum5d float64, days int) {
	days = len(recentCloses)
	if days == 0 {
		return 0, 0, 0
	}
	// 均线 = 最近5日 (不足5日时按实际天数) 均价, 不含今天
	sum := 0.0
	count := 0
	for _, c := range recentCloses {
		if count >= 5 {
			break
		}
		sum += c
		count++
	}
	ma5 = sum / float64(count)

	// 动量 = (今天收盘 - N日前收盘) / N日前收盘 * 100
	oldest := recentCloses[days-1]
	if oldest > 0 {
		momentum5d = (todayClose - oldest) / oldest * 100
	}
	return ma5, momentum5d, days
}

// buildReason 构建入选理由
func (s *Screener) buildReason(basic model.DailyBasic, pctChg, ma5, momentum5d float64, trendDays int) string {
	var parts []string
	if basic.TurnoverRate >= 3 {
		parts = append(parts, fmt.Sprintf("换手率%.1f%%(活跃)", basic.TurnoverRate))
	}
	if basic.VolumeRatio >= 1.5 {
		parts = append(parts, fmt.Sprintf("量比%.1f(放量)", basic.VolumeRatio))
	}
	if basic.PE_TTM > 0 && basic.PE_TTM <= 30 {
		parts = append(parts, fmt.Sprintf("PE_TTM=%.1f(估值合理)", basic.PE_TTM))
	}
	if basic.PB > 0 && basic.PB <= 2 {
		parts = append(parts, fmt.Sprintf("PB=%.2f(破净或低估值)", basic.PB))
	}
	if pctChg > 0 && pctChg <= 5 {
		parts = append(parts, fmt.Sprintf("涨%.1f%%(温和上涨)", pctChg))
	}
	if basic.CircMV > 0 {
		mvYi := basic.CircMV / 10000 // 万元→亿元
		parts = append(parts, fmt.Sprintf("流通市值%.0f亿", mvYi))
	}
	if ma5 > 0 && trendDays >= 3 { // 数据不足 3 日时省略均线文案
		if basic.Close > ma5 {
			parts = append(parts, fmt.Sprintf("MA%d=%.2f(线上)", trendDays, ma5))
		}
	}
	if trendDays >= 2 {
		parts = append(parts, fmt.Sprintf("%d日动量%.1f%%", trendDays, momentum5d))
	}
	if len(parts) == 0 {
		return "综合评分入选"
	}
	return strings.Join(parts, ", ")
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

// GetLatestResults 获取最新选股结果
func (s *Screener) GetLatestResults() ([]ScreenResult, error) {
	return s.screenRepo.GetLatest()
}
