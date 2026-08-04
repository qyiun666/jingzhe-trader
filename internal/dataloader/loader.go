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

// watchCodes 获取关注股票代码集合(实例级缓存)
func (l *Loader) watchCodes() map[string]bool {
	if l.watchCache != nil {
		return l.watchCache
	}
	l.watchCache = l.buildWatchCodeSet()
	return l.watchCache
}

// buildWatchCodeSet 构建关注的股票代码集合
// 包含: 股票池(bluechip + tech) + watchlist 配置 + 当前持仓
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
	return codeSet
}
