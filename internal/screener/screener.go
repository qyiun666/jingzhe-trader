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
// 2. 多维度筛选
// 3. 评分排序取 TopN
// 4. 同步候选股票历史K线 (策略均线计算需要)
// 5. 结果落库
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

		// 评分: 换手率(活跃度) + 量比(资金关注度) + 估值吸引力
		score := s.calcScore(basic, pctChg)

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
			Reason:       s.buildReason(basic, pctChg),
		})
	}

	logger.L().Infow("初筛完成", "count", len(candidates))

	// 6. 按评分排序取 TopN
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	if len(candidates) > s.cfg.MaxCandidates {
		candidates = candidates[:s.cfg.MaxCandidates]
	}

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

// calcScore 评分: 综合活跃度、资金关注度、估值吸引力
func (s *Screener) calcScore(basic model.DailyBasic, pctChg float64) float64 {
	// 活跃度: 换手率 (1-10% 最佳, 过高可能是炒作)
	turnoverScore := 0.0
	if basic.TurnoverRate > 0 {
		if basic.TurnoverRate <= 10 {
			turnoverScore = basic.TurnoverRate // 线性
		} else {
			turnoverScore = 10 - (basic.TurnoverRate-10)*0.5 // 超过10%递减
		}
	}

	// 资金关注度: 量比 (>1 表示放量)
	volRatioScore := 0.0
	if basic.VolumeRatio > 0 {
		if basic.VolumeRatio <= 3 {
			volRatioScore = basic.VolumeRatio
		} else {
			volRatioScore = 3 // 封顶
		}
	}

	// 估值吸引力: PE_TTM 越合理越好 (10-30 最佳)
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

	// 涨跌幅: 小涨优于大跌 (避免接飞刀)
	chgScore := 0.0
	if pctChg > 0 && pctChg <= 5 {
		chgScore = 2.0
	} else if pctChg > 5 && pctChg <= 9 {
		chgScore = 1.0
	} else if pctChg < -3 {
		chgScore = -1.0 // 大跌扣分
	}

	return turnoverScore*0.3 + volRatioScore*0.3 + peScore*0.25 + chgScore*0.15
}

// buildReason 构建入选理由
func (s *Screener) buildReason(basic model.DailyBasic, pctChg float64) string {
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

// GetLatestResults 获取最新选股结果
func (s *Screener) GetLatestResults() ([]ScreenResult, error) {
	return s.screenRepo.GetLatest()
}
