package dataloader

import (
	"fmt"
	"sort"
	"strings"
	"sync"
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

// dataloaderConcurrency 数据同步并发 worker 数
// 收益来自 tushare HTTP 请求往返并行 (限流由 tushare.Client 令牌桶统一控制)
// 写入受 SQLite 单连接 (SetMaxOpenConns(1)) 串行化, 不产生额外锁竞争
const dataloaderConcurrency = 4

// Report 一次数据同步的结果
// OptionalFailures 记录辅助数据源 (新闻/资金流/龙虎榜/财务指标) 的失败原因。
// 它们刻意不与核心同步的 error 混为一谈: 辅助源不可用不影响行情与信号正确性,
// 若一并返回 error 会让调度器把 data_update 判为失败, 进而中止当天信号生成 (宁缺毋滥逻辑)。
// 但必须可见 —— 此前只记日志, 结果就是四张辅助表长期 0 行而任务一直显示成功。
type Report struct {
	OptionalFailures []string
}

// Run 执行数据同步: 核心数据失败返回 error, 辅助数据失败记入 Report 不影响返回值
func (l *Loader) Run(opts Options) (Report, error) {
	if l.cfg.Tushare.Token == "" {
		return Report{}, fmt.Errorf("未配置 tushare.token (可用环境变量 TUSHARE_TOKEN)")
	}
	defer l.ts.Close() // 进程内短生命周期 Loader: 用完停掉限流补充 goroutine, 否则每次调用泄漏一个
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
		return Report{}, fmt.Errorf("查询交易日失败: %w", err)
	}
	l.syncIndexHistory(opts.StartDate, opts.EndDate)
	l.syncDailyData(tradeCals)
	l.backfillAdjFactors()

	report := Report{OptionalFailures: l.syncOptional(opts, tradeCals)}
	logger.L().Info("数据同步全部完成!")
	return report, nil
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

// syncDailyData 按交易日同步日线/涨跌停/基本面/ETF (交易日间并行拉取)
// 除增量同步外, 每个交易日重拉最近 StaleKeepRecentDays 自然日 (UPSERT覆盖),
// 以吸收 tushare 对近期数据的更正 (分红/停复牌/行情修订)
func (l *Loader) syncDailyData(tradeCals []model.TradeCal) {
	logger.L().Info("=== 同步日线行情 ===")
	lastDate, _ := l.barRepo.GetMaxTradeDate()
	reSyncFrom := ""
	if lastDate != "" {
		if t, err := time.Parse("20060102", lastDate); err == nil {
			// 回退天数直接取全市场保留窗口: 重拉窗口一旦窄于它, 停机期间落在两者之间的那些日期
			// 就永远补不回来 —— 它们承诺保留全市场却再没人重拉, 只会停在裁剪状态
			reSyncFrom = t.AddDate(0, 0, -store.StaleKeepRecentDays).Format("20060102")
		}
	}

	// 预热 watchCodes 缓存: 并发期间各 goroutine 只读共享 map, 避免写入竞争
	l.watchCodes()

	var mu sync.Mutex
	syncedCount := 0
	reSyncedCount := 0

	var wg sync.WaitGroup
	sem := make(chan struct{}, dataloaderConcurrency)
	for _, cal := range tradeCals {
		calDate := cal.CalDate
		isResync := false
		if lastDate != "" && calDate <= lastDate {
			if reSyncFrom != "" && calDate >= reSyncFrom {
				// 重拉窗口内: 覆盖式刷新近期数据 (吸收数据源更正)
				isResync = true
			} else {
				continue // 跳过历史日期
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if isResync {
				mu.Lock()
				reSyncedCount++
				mu.Unlock()
				logger.L().Debugf("重拉 %s 日线(覆盖更正)...", calDate)
			} else {
				logger.L().Infof("同步 %s 日线...", calDate)
			}

			if !l.syncOneDayBars(calDate) {
				return
			}
			l.syncOneDayExtras(calDate)

			mu.Lock()
			syncedCount++
			if (syncedCount+reSyncedCount)%10 == 0 {
				logger.L().Infof("已同步 %d 个交易日", syncedCount+reSyncedCount)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	logger.L().Infof("日线行情同步完成, 新增 %d 个交易日, 重拉覆盖 %d 个交易日", syncedCount, reSyncedCount)
}

// validateBars K线入库校验: 剔除明显错误的数据行 (价格非正/高低倒挂/成交量为负)
// 返回有效行数; 剔除时记日志告警, 剔除比例过高时返回 false 让调用方放弃整日写入
func validateBars(bars []model.Bar, calDate string) ([]model.Bar, bool) {
	valid := bars[:0]
	dropped := 0
	for _, b := range bars {
		if b.Close <= 0 || b.Open <= 0 || b.High < b.Low || b.Vol < 0 || b.High < b.Close || b.Low > b.Close {
			dropped++
			logger.L().Warnf("数据校验剔除异常K线 %s %s: O%.2f H%.2f L%.2f C%.2f V%.0f",
				b.TsCode, b.TradeDate, b.Open, b.High, b.Low, b.Close, b.Vol)
			continue
		}
		valid = append(valid, b)
	}
	if dropped > 0 {
		logger.L().Warnf("%s 数据校验: 剔除 %d/%d 条异常K线", calDate, dropped, len(bars))
	}
	// 剔除比例超 5% 说明数据源整体异常, 不信任整日数据
	if len(bars) > 20 && dropped*20 > len(bars) {
		logger.L().Errorf("%s 异常K线比例过高 (%d/%d), 放弃整日写入", calDate, dropped, len(bars))
		return nil, false
	}
	return valid, true
}

// syncOneDayBars 同步单日日线, 返回是否成功
func (l *Loader) syncOneDayBars(calDate string) bool {
	bars, err := l.ts.Daily(calDate)
	if err != nil {
		logger.L().Errorf("获取 %s 日线失败: %v", calDate, err)
		return false
	}
	// 当日无数据视为未同步成功: 数据源延迟时返回空, 若按成功记录会跳过重试窗口造成"假成功"
	if len(bars) == 0 {
		logger.L().Warnf("获取 %s 日线为空 (数据源可能未更新), 本次同步视为失败以触发重试", calDate)
		return false
	}
	// 合并当日复权因子 (daily 接口不返回, 需单独拉取; 失败不阻断, 由 backfillAdjFactors 兜底)
	l.mergeAdjFactors(bars, calDate)
	// 入库前校验
	var ok bool
	if bars, ok = validateBars(bars, calDate); !ok {
		return false
	}
	// filter_mode 只裁剪历史日期: 近 StaleKeepRecentDays 内必须留全市场日线,
	// 否则选股器按日读本地收盘价算 MA5 时只剩股票池里那几个代码 (多为 ETF),
	// 趋势过滤与 5 日动量对全部 A 股候选静默失效 (与 CleanStaleStocks 同一窗口口径)
	if l.trimHistory(calDate) {
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
			// 与日线同一裁剪口径: 选股器 (screener) 与信号适配 (data_adapter) 都按日取
			// daily_basic 全市场, 把近窗口也裁成关注集等于让候选池只剩股票池自己
			if l.trimHistory(calDate) {
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
		fundBars, err := l.ts.FundDaily(calDate)
		if err != nil {
			logger.L().Errorf("获取 %s ETF日线失败: %v", calDate, err)
		} else if len(fundBars) == 0 {
			// 空结果必须留痕: 曾因静默跳过导致持仓ETF缺当日数据被误判停牌/低估
			logger.L().Warnf("获取 %s ETF日线为空 (数据源延迟或未更新), 持仓ETF估值将停留上一交易日", calDate)
		} else if err := l.barRepo.BatchInsert(fundBars); err != nil {
			logger.L().Errorf("存储 %s ETF日线失败: %v", calDate, err)
		}
	}

	// 同步 watchlist 中的指数日线 (如 000300.SH 大盘过滤需要)
	// 按代码逐个拉而非按交易日拉全市场: index_daily 按 trade_date 查是"全部指数"口径
	// (实测单次返回 8000 行, 已到该接口分页上限), 只为筛出 watchlist 里那一两个代码,
	// 既白耗低档位的调用计次, 目标指数也可能被截断掉直接漏数据。
	for _, code := range sortedKeys(l.indexCodes()) {
		bars, err := l.ts.IndexDailyByCode(code, calDate, calDate)
		if err != nil {
			logger.L().Errorf("获取 %s %s 指数日线失败: %v", code, calDate, err)
			continue
		}
		if len(bars) == 0 {
			// 空结果留痕: 大盘过滤器会继续沿用上一交易日收盘, 表现与"指数在跌"难以区分
			logger.L().Warnf("获取 %s %s 指数日线为空 (数据源未更新)", code, calDate)
			continue
		}
		if err := l.barRepo.BatchInsert(bars); err != nil {
			logger.L().Errorf("存储 %s %s 指数日线失败: %v", code, calDate, err)
		}
	}
}

// SyncCalendarOnly 仅同步交易日历 (轻量级, 用于打破调度器日历死锁)
// 当调度器发现今日不在日历中时调用, 同步近期日历后即可正常判断交易日
func (l *Loader) SyncCalendarOnly(start, end string) error {
	if l.cfg.Tushare.Token == "" {
		return fmt.Errorf("未配置 tushare.token (可用环境变量 TUSHARE_TOKEN)")
	}
	defer l.ts.Close()
	l.syncCalendar(start, end)
	return nil
}

// syncIndexHistory 同步 watchlist 中指数的历史日线 (按代码并行拉取, 不依赖交易日遍历)
func (l *Loader) syncIndexHistory(start, end string) {
	indexCodes := l.indexCodes()
	if len(indexCodes) == 0 {
		return
	}
	logger.L().Infof("=== 同步指数历史日线 (%d个) ===", len(indexCodes))

	var wg sync.WaitGroup
	sem := make(chan struct{}, dataloaderConcurrency)
	for code := range indexCodes {
		wg.Add(1)
		go func(code string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// 直接走 index_daily: 这里的 code 全部来自 indexCodes() (即 watchlist 中的指数),
			// 而 daily 接口对指数代码返回 0 行 —— 先按 daily 试一次只会每轮白耗一次请求
			bars, err := l.ts.IndexDailyByCode(code, start, end)
			if err != nil {
				logger.L().Errorf("获取 %s 指数日线失败: %v", code, err)
				return
			}
			if err := l.barRepo.BatchInsert(bars); err != nil {
				logger.L().Errorf("存储 %s 指数日线失败: %v", code, err)
				return
			}
			logger.L().Infof("  %s: %d 条", code, len(bars))
		}(code)
	}
	wg.Wait()
}

// trimHistory 该交易日的行情是否可按关注集裁剪存储
//
// FilterMode 的裁剪只能作用于"选股再也用不到"的历史日期: 日线与每日基本面都有按日取全市场的
// 消费方 (screener 的市值/PE/换手筛选、data_adapter 的当日基本面、动量与趋势的近5日收盘),
// 近窗口内裁成关注集就等于把候选池缩到股票池自己那几只, 筛选条件对全部 A 股静默失效。
// 窗口与 CleanStaleStocks 共用 store.StaleKeepRecentDays, 避免两处口径分叉。
// 只按代码取数的 (如 stk_limit, 自带 CalcUpLimit 兜底) 不受此约束, 仍按 FilterMode 全量裁剪。
func (l *Loader) trimHistory(calDate string) bool {
	if !l.cfg.Dataloader.FilterMode {
		return false
	}
	return calDate < time.Now().AddDate(0, 0, -store.StaleKeepRecentDays).Format("20060102")
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

// IsIndexCode 判断代码是否大盘指数 (000xxx.SH 或 399xxx.SZ)
// 000300.SH 沪深300, 000001.SH 上证综指, 399001.SZ 深证成指 等
// 导出供 api 层给大盘分析师注入指数清单时复用同一判定
func IsIndexCode(code string) bool {
	code = strings.TrimSpace(code)
	if code == "" {
		return false
	}
	return (strings.HasPrefix(code, "000") || strings.HasPrefix(code, "399")) &&
		(strings.HasSuffix(code, ".SH") || strings.HasSuffix(code, ".SZ"))
}

// indexCodes 从 watchlist 中提取指数代码
func (l *Loader) indexCodes() map[string]bool {
	codeSet := make(map[string]bool)
	for _, code := range l.cfg.Dataloader.Watchlist {
		if IsIndexCode(code) {
			codeSet[strings.TrimSpace(code)] = true
		}
	}
	return codeSet
}

// sortedKeys 集合转升序切片: 让遍历顺序与日志进度可复现 (map 迭代顺序随机)
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
