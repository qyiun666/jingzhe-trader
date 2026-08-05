package dataloader

import (
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
	"jingzhe-trader/pkg/logger"
)

// Options 数据同步选项
type Options struct {
	StartDate     string // 起始日期 YYYYMMDD, 空则默认3年前
	EndDate       string // 结束日期 YYYYMMDD, 空则默认今天
	SyncNewShare  bool   // 同步新股申购
	SyncNews      bool   // 同步新闻快讯
	SyncMoneyFlow bool   // 同步资金流向
	SyncTopList   bool   // 同步龙虎榜
	SyncFina      bool   // 同步财务指标
}

// Loader 数据加载器
// 供 CLI (cmd/dataloader) 与调度器进程内调用共用
type Loader struct {
	cfg           *config.Config
	db            *sqlx.DB
	ts            *tushare.Client
	stockRepo     *store.StockRepo
	calRepo       *store.CalendarRepo
	barRepo       *store.BarRepo
	limitRepo     *store.LimitRepo
	basicRepo     *store.BasicRepo
	portfolioRepo *store.PortfolioRepo
	watchCache    map[string]bool
}

// New 创建数据加载器
func New(cfg *config.Config, db *sqlx.DB) *Loader {
	return &Loader{
		cfg:           cfg,
		db:            db,
		ts:            tushare.NewClient(cfg.Tushare),
		stockRepo:     store.NewStockRepo(db),
		calRepo:       store.NewCalendarRepo(db),
		barRepo:       store.NewBarRepo(db),
		limitRepo:     store.NewLimitRepo(db),
		basicRepo:     store.NewBasicRepo(db),
		portfolioRepo: store.NewPortfolioRepo(db),
	}
}

// Run 执行数据同步 (核心 + 可选项)
func (l *Loader) Run(opts Options) error {
	if l.cfg.Tushare.Token == "" {
		return fmt.Errorf("未配置 tushare.token (可用环境变量 TUSHARE_TOKEN)")
	}
	if opts.StartDate == "" {
		opts.StartDate = time.Now().AddDate(-3, 0, 0).Format("20060102")
	}
	if opts.EndDate == "" {
		opts.EndDate = time.Now().Format("20060102")
	}

	l.syncCalendar(opts.StartDate, opts.EndDate)
	l.syncStockList()

	tradeCals, err := l.calRepo.GetTradeDays(opts.StartDate, opts.EndDate)
	if err != nil {
		return fmt.Errorf("查询交易日失败: %w", err)
	}
	l.syncIndexHistory(opts.StartDate, opts.EndDate)
	l.syncDailyData(tradeCals)
	l.syncOptional(opts, tradeCals)

	logger.L().Info("数据同步全部完成!")
	return nil
}

// syncCalendar 同步交易日历
func (l *Loader) syncCalendar(startDate, endDate string) {
	logger.L().Info("=== 同步交易日历 ===")
	cals, err := l.ts.TradeCal("SSE", startDate, endDate)
	if err != nil {
		logger.L().Errorf("获取交易日历失败: %v", err)
		return
	}
	if err := l.calRepo.BatchInsert(cals); err != nil {
		logger.L().Errorf("存储交易日历失败: %v", err)
		return
	}
	logger.L().Infof("交易日历同步完成: %d 条", len(cals))
}

// syncStockList 同步股票列表
func (l *Loader) syncStockList() {
	logger.L().Info("=== 同步股票列表 ===")
	stocks, err := l.ts.StockBasic()
	if err != nil {
		logger.L().Errorf("获取股票列表失败: %v", err)
		return
	}
	if err := l.stockRepo.BatchInsert(stocks); err != nil {
		logger.L().Errorf("存储股票列表失败: %v", err)
		return
	}
	logger.L().Infof("股票列表同步完成: %d 只", len(stocks))
}

// syncDailyData 按交易日同步日线/涨跌停/基本面/ETF
func (l *Loader) syncDailyData(tradeCals []model.TradeCal) {
	logger.L().Info("=== 同步日线行情 ===")
	lastDate, _ := l.barRepo.GetMaxTradeDate()
	syncedCount := 0

	for _, cal := range tradeCals {
		if lastDate != "" && cal.CalDate <= lastDate {
			continue // 跳过已同步的日期
		}
		logger.L().Infof("同步 %s 日线...", cal.CalDate)

		if !l.syncOneDayBars(cal.CalDate) {
			continue
		}
		l.syncOneDayExtras(cal.CalDate)

		syncedCount++
		if syncedCount%10 == 0 {
			logger.L().Infof("已同步 %d 个交易日", syncedCount)
		}
	}
	logger.L().Infof("日线行情同步完成, 共 %d 个交易日", syncedCount)
}

// syncOneDayBars 同步单日日线, 返回是否成功
func (l *Loader) syncOneDayBars(calDate string) bool {
	bars, err := l.ts.Daily(calDate)
	if err != nil {
		logger.L().Errorf("获取 %s 日线失败: %v", calDate, err)
		return false
	}
	if l.cfg.Dataloader.FilterMode {
		watchCodes := l.watchCodes()
		filtered := make([]model.Bar, 0, len(bars))
		for _, bar := range bars {
			if watchCodes[bar.TsCode] {
				filtered = append(filtered, bar)
			}
		}
		bars = filtered
	}
	if err := l.barRepo.BatchInsert(bars); err != nil {
		logger.L().Errorf("存储 %s 日线失败: %v", calDate, err)
		return false
	}
	return true
}

// syncOneDayExtras 同步单日涨跌停/基本面/ETF日线
func (l *Loader) syncOneDayExtras(calDate string) {
	if l.cfg.Dataloader.EnableLimit {
		if limits, err := l.ts.StkLimit(calDate); err == nil && len(limits) > 0 {
			if l.cfg.Dataloader.FilterMode {
				watchCodes := l.watchCodes()
				filtered := make([]model.StkLimit, 0, len(limits))
				for _, lim := range limits {
					if watchCodes[lim.TsCode] {
						filtered = append(filtered, lim)
					}
				}
				limits = filtered
			}
			if err := l.limitRepo.BatchInsert(limits); err != nil {
				logger.L().Errorf("存储 %s 涨跌停失败: %v", calDate, err)
			}
		}
	}

	if l.cfg.Dataloader.EnableBasic {
		if basics, err := l.ts.DailyBasic(calDate); err == nil && len(basics) > 0 {
			if l.cfg.Dataloader.FilterMode {
				watchCodes := l.watchCodes()
				filtered := make([]model.DailyBasic, 0, len(basics))
				for _, b := range basics {
					if watchCodes[b.TsCode] {
						filtered = append(filtered, b)
					}
				}
				basics = filtered
			}
			if err := l.basicRepo.BatchInsert(basics); err != nil {
				logger.L().Errorf("存储 %s 基本面失败: %v", calDate, err)
			}
		}
	}

	if l.cfg.Dataloader.EnableFund {
		if fundBars, err := l.ts.FundDaily(calDate); err == nil && len(fundBars) > 0 {
			if err := l.barRepo.BatchInsert(fundBars); err != nil {
				logger.L().Errorf("存储 %s ETF日线失败: %v", calDate, err)
			}
		}
	}

	// 同步 watchlist 中的指数日线 (如 000300.SH 大盘过滤需要)
	indexCodes := l.indexCodes()
	if len(indexCodes) > 0 {
		if idxBars, err := l.ts.IndexDaily(calDate); err == nil && len(idxBars) > 0 {
			filtered := make([]model.Bar, 0, len(indexCodes))
			for _, bar := range idxBars {
				if indexCodes[bar.TsCode] {
					filtered = append(filtered, bar)
				}
			}
			if len(filtered) > 0 {
				if err := l.barRepo.BatchInsert(filtered); err != nil {
					logger.L().Errorf("存储 %s 指数日线失败: %v", calDate, err)
				}
			}
		}
	}
}

// SyncCalendarOnly 仅同步交易日历 (轻量级, 用于打破调度器日历死锁)
// 当调度器发现今日不在日历中时调用, 同步近期日历后即可正常判断交易日
func (l *Loader) SyncCalendarOnly(start, end string) error {
	if l.cfg.Tushare.Token == "" {
		return fmt.Errorf("未配置 tushare.token (可用环境变量 TUSHARE_TOKEN)")
	}
	l.syncCalendar(start, end)
	return nil
}

// syncIndexHistory 同步 watchlist 中指数的历史日线 (按代码拉取, 不依赖交易日遍历)
func (l *Loader) syncIndexHistory(start, end string) {
	indexCodes := l.indexCodes()
	if len(indexCodes) == 0 {
		return
	}
	logger.L().Infof("=== 同步指数历史日线 (%d个) ===", len(indexCodes))
	for code := range indexCodes {
		bars, err := l.ts.DailyByCode(code, start, end)
		if err != nil {
			logger.L().Errorf("获取 %s 指数日线失败: %v", code, err)
			continue
		}
		// DailyByCode 用的是 daily 接口, 指数需要 index_daily
		// 这里直接用 IndexDailyByCode
		if len(bars) == 0 {
			bars, err = l.ts.IndexDailyByCode(code, start, end)
			if err != nil {
				logger.L().Errorf("获取 %s 指数日线(index)失败: %v", code, err)
				continue
			}
		}
		if err := l.barRepo.BatchInsert(bars); err != nil {
			logger.L().Errorf("存储 %s 指数日线失败: %v", code, err)
			continue
		}
		logger.L().Infof("  %s: %d 条", code, len(bars))
	}
}

// watchCodes 获取关注股票代码集合(实例级缓存)
func (l *Loader) watchCodes() map[string]bool {
	if l.watchCache != nil {
		return l.watchCache
	}
	l.watchCache = l.buildWatchCodeSet()
	return l.watchCache
}

// buildWatchCodeSet 构建关注的股票代码集合
// 包含: 股票池(可选) + watchlist + 持仓 + 自动选股结果
func (l *Loader) buildWatchCodeSet() map[string]bool {
	codeSet := make(map[string]bool)
	addCodes := func(csv string) {
		for _, code := range strings.Split(csv, ",") {
			if code = strings.TrimSpace(code); code != "" {
				codeSet[code] = true
			}
		}
	}
	addCodes(l.cfg.Universe.Bluechip)
	addCodes(l.cfg.Universe.Tech)
	for _, code := range l.cfg.Dataloader.Watchlist {
		if code = strings.TrimSpace(code); code != "" {
			codeSet[code] = true
		}
	}
	if positions, err := l.portfolioRepo.GetAllPositions(); err == nil {
		for _, p := range positions {
			codeSet[p.TsCode] = true
		}
	}
	// 合并自动选股结果 (选股器每日筛选的候选股票)
	if rows, err := l.db.Queryx("SELECT DISTINCT ts_code FROM screen_result"); err == nil {
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err == nil && code != "" {
				codeSet[code] = true
			}
		}
		rows.Close()
	}
	return codeSet
}

// indexCodes 从 watchlist 中提取指数代码 (以 .SH 结尾且以 0/3 开头的通常是指数)
// 000300.SH 沪深300, 000001.SH 上证综指, 399001.SZ 深证成指 等
func (l *Loader) indexCodes() map[string]bool {
	codeSet := make(map[string]bool)
	for _, code := range l.cfg.Dataloader.Watchlist {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}
		// 指数代码: 000xxx.SH 或 399xxx.SZ
		if (strings.HasPrefix(code, "000") || strings.HasPrefix(code, "399")) &&
			(strings.HasSuffix(code, ".SH") || strings.HasSuffix(code, ".SZ")) {
			codeSet[code] = true
		}
	}
	return codeSet
}
