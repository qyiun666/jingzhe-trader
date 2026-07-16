package main

import (
	"flag"
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

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	startDate := flag.String("start", "", "起始日期 YYYYMMDD (留空则从上次同步位置继续)")
	endDate := flag.String("end", time.Now().Format("20060102"), "结束日期 YYYYMMDD")

	// 可选数据同步开关
	syncNewShare := flag.Bool("newshare", false, "同步新股申购数据")
	syncNews := flag.Bool("news", false, "同步新闻快讯")
	syncMoneyFlow := flag.Bool("moneyflow", false, "同步个股资金流向")
	syncTopList := flag.Bool("toplist", false, "同步龙虎榜数据")
	syncFina := flag.Bool("fina", false, "同步财务指标数据(按报告期获取, 每季度采集一次)")
	cleanup := flag.Bool("cleanup", false, "清理不在股票池和持仓中的股票数据")
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		return
	}

	// 初始化日志
	logger.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.Output, cfg.Log.FilePath)
	defer logger.Sync()

	if cfg.Tushare.Token == "" {
		fmt.Println("错误: 请在配置文件中设置 tushare.token")
		return
	}

	// 初始化数据库
	db, err := store.NewDB(cfg.Database.Path)
	if err != nil {
		logger.L().Fatalf("初始化数据库失败: %v", err)
	}
	defer db.Close()

	// 初始化Tushare客户端
	tsClient := tushare.NewClient(cfg.Tushare)

	// 初始化Repo
	stockRepo := store.NewStockRepo(db)
	calRepo := store.NewCalendarRepo(db)
	barRepo := store.NewBarRepo(db)
	limitRepo := store.NewLimitRepo(db)
	basicRepo := store.NewBasicRepo(db)
	portfolioRepo := store.NewPortfolioRepo(db)

	// 0. 清理模式: 直接清理数据后退出
	if *cleanup {
		logger.L().Info("=== 清理多余股票数据 ===")
		watchCodes := buildWatchCodeSet(cfg, stockRepo, portfolioRepo)
		logger.L().Infof("关注股票数: %d 只", len(watchCodes))
		deletedBars, deletedLimits, deletedBasics, err := cleanupUnusedData(db, watchCodes)
		if err != nil {
			logger.L().Errorf("清理数据失败: %v", err)
		} else {
			logger.L().Infof("清理完成: 删除 %d 条日线, %d 条涨跌停, %d 条基本面",
				deletedBars, deletedLimits, deletedBasics)
		}
		// 执行 VACUUM 回收空间
		if _, err := db.Exec("VACUUM"); err != nil {
			logger.L().Warnf("VACUUM 失败: %v", err)
		} else {
			logger.L().Info("VACUUM 完成, 空间已回收")
		}
		return
	}

	// 1. 同步交易日历
	logger.L().Info("=== 同步交易日历 ===")
	if *startDate == "" {
		*startDate = time.Now().AddDate(-3, 0, 0).Format("20060102")
	}
	cals, err := tsClient.TradeCal("SSE", *startDate, *endDate)
	if err != nil {
		logger.L().Errorf("获取交易日历失败: %v", err)
	} else {
		if err := calRepo.BatchInsert(cals); err != nil {
			logger.L().Errorf("存储交易日历失败: %v", err)
		} else {
			logger.L().Infof("交易日历同步完成: %d 条", len(cals))
		}
	}

	// 2. 同步股票列表
	logger.L().Info("=== 同步股票列表 ===")
	stocks, err := tsClient.StockBasic()
	if err != nil {
		logger.L().Errorf("获取股票列表失败: %v", err)
	} else {
		if err := stockRepo.BatchInsert(stocks); err != nil {
			logger.L().Errorf("存储股票列表失败: %v", err)
		} else {
			logger.L().Infof("股票列表同步完成: %d 只", len(stocks))
		}
	}

	// 3. 同步日线行情
	logger.L().Info("=== 同步日线行情 ===")
	// 获取需要同步的交易日
	tradeCals, err := calRepo.GetTradeDays(*startDate, *endDate)
	if err != nil {
		logger.L().Fatalf("查询交易日失败: %v", err)
	}

	// 检查已同步到的最后日期
	lastDate, _ := barRepo.GetMaxTradeDate()
	syncedCount := 0

	for _, cal := range tradeCals {
		if lastDate != "" && cal.CalDate <= lastDate {
			continue // 跳过已同步的日期
		}

		logger.L().Infof("同步 %s 日线...", cal.CalDate)

		// 日线行情
		bars, err := tsClient.Daily(cal.CalDate)
		if err != nil {
			logger.L().Errorf("获取 %s 日线失败: %v", cal.CalDate, err)
			continue
		}
		// 筛选模式: 只保留关注的股票
		if cfg.Dataloader.FilterMode {
			watchCodes := getWatchCodeCache(cfg, stockRepo, portfolioRepo)
			filtered := make([]model.Bar, 0, len(bars))
			for _, bar := range bars {
				if watchCodes[bar.TsCode] {
					filtered = append(filtered, bar)
				}
			}
			bars = filtered
		}
		if err := barRepo.BatchInsert(bars); err != nil {
			logger.L().Errorf("存储 %s 日线失败: %v", cal.CalDate, err)
			continue
		}

		// 涨跌停价
		if cfg.Dataloader.EnableLimit {
			limits, err := tsClient.StkLimit(cal.CalDate)
			if err == nil && len(limits) > 0 {
				if cfg.Dataloader.FilterMode {
					watchCodes := getWatchCodeCache(cfg, stockRepo, portfolioRepo)
					filtered := make([]model.StkLimit, 0, len(limits))
					for _, lim := range limits {
						if watchCodes[lim.TsCode] {
							filtered = append(filtered, lim)
						}
					}
					limits = filtered
				}
				limitRepo.BatchInsert(limits)
			}
		}

		// 每日基本面
		if cfg.Dataloader.EnableBasic {
			basics, err := tsClient.DailyBasic(cal.CalDate)
			if err == nil && len(basics) > 0 {
				if cfg.Dataloader.FilterMode {
					watchCodes := getWatchCodeCache(cfg, stockRepo, portfolioRepo)
					filtered := make([]model.DailyBasic, 0, len(basics))
					for _, b := range basics {
						if watchCodes[b.TsCode] {
							filtered = append(filtered, b)
						}
					}
					basics = filtered
				}
				basicRepo.BatchInsert(basics)
			}
		}

		// ETF/基金日线(与股票日线共用 daily_bar 表, ts_code 可区分)
		if cfg.Dataloader.EnableFund {
			fundBars, err := tsClient.FundDaily(cal.CalDate)
			if err == nil && len(fundBars) > 0 {
				if err := barRepo.BatchInsert(fundBars); err != nil {
					logger.L().Errorf("存储 %s ETF日线失败: %v", cal.CalDate, err)
				}
			}
		}

		syncedCount++
		if syncedCount%10 == 0 {
			logger.L().Infof("已同步 %d 个交易日", syncedCount)
		}
	}

	logger.L().Infof("日线行情同步完成, 共 %d 个交易日", syncedCount)

	// 4. 同步新股申购数据(可选)
	if *syncNewShare {
		logger.L().Info("=== 同步新股申购数据 ===")
		newShares, err := tsClient.NewShare(*startDate, *endDate)
		if err != nil {
			logger.L().Errorf("获取新股申购数据失败: %v", err)
		} else {
			nsRepo := store.NewNewShareRepo(db)
			if err := nsRepo.BatchInsert(newShares); err != nil {
				logger.L().Errorf("存储新股申购数据失败: %v", err)
			} else {
				logger.L().Infof("新股申购数据同步完成: %d 条", len(newShares))
			}
		}
	}

	// 5. 同步新闻快讯(可选)
	if *syncNews {
		logger.L().Info("=== 同步新闻快讯 ===")
		newsList, err := tsClient.MajorNews(*startDate, *endDate, "")
		if err != nil {
			logger.L().Errorf("获取新闻快讯失败: %v", err)
		} else {
			newsRepo := store.NewNewsRepo(db)
			if err := newsRepo.BatchInsert(newsList); err != nil {
				logger.L().Errorf("存储新闻快讯失败: %v", err)
			} else {
				logger.L().Infof("新闻快讯同步完成: %d 条", len(newsList))
			}
		}
	}

	// 6. 同步个股资金流向(可选, 按交易日)
	if *syncMoneyFlow {
		logger.L().Info("=== 同步个股资金流向 ===")
		mfRepo := store.NewMoneyFlowRepo(db)
		lastMFDate, _ := mfRepo.GetMaxTradeDate()
		mfSynced := 0
		for _, cal := range tradeCals {
			if lastMFDate != "" && cal.CalDate <= lastMFDate {
				continue
			}
			flows, err := tsClient.MoneyFlow(cal.CalDate)
			if err != nil {
				logger.L().Errorf("获取 %s 资金流向失败: %v", cal.CalDate, err)
				continue
			}
			if len(flows) == 0 {
				continue
			}
			if err := mfRepo.BatchInsert(flows); err != nil {
				logger.L().Errorf("存储 %s 资金流向失败: %v", cal.CalDate, err)
				continue
			}
			mfSynced++
		}
		logger.L().Infof("个股资金流向同步完成, 共 %d 个交易日", mfSynced)
	}

	// 7. 同步龙虎榜(可选, 按交易日)
	if *syncTopList {
		logger.L().Info("=== 同步龙虎榜 ===")
		tlRepo := store.NewTopListRepo(db)
		lastTLDate, _ := tlRepo.GetMaxTradeDate()
		tlSynced := 0
		for _, cal := range tradeCals {
			if lastTLDate != "" && cal.CalDate <= lastTLDate {
				continue
			}
			list, err := tsClient.TopList(cal.CalDate)
			if err != nil {
				logger.L().Errorf("获取 %s 龙虎榜失败: %v", cal.CalDate, err)
				continue
			}
			if len(list) == 0 {
				continue
			}
			if err := tlRepo.BatchInsert(list); err != nil {
				logger.L().Errorf("存储 %s 龙虎榜失败: %v", cal.CalDate, err)
				continue
			}
			tlSynced++
		}
		logger.L().Infof("龙虎榜同步完成, 共 %d 个交易日", tlSynced)
	}

	// 8. 同步财务指标(可选, 逐股票获取)
	// Tushare 500元档 fina_indicator 必须传 ts_code, 不能按报告期批量获取
	if *syncFina {
		logger.L().Info("=== 同步财务指标 ===")
		finaRepo := store.NewFinaRepo(db)

		// 获取所有上市股票
		stockRepo := store.NewStockRepo(db)
		allStocks, err := stockRepo.GetAll()
		if err != nil || len(allStocks) == 0 {
			logger.L().Errorf("获取股票列表失败: %v", err)
		} else {
			// 生成需要同步的报告期列表
			periods := genReportPeriods(*startDate, *endDate)
			logger.L().Infof("待同步报告期: %v", periods)
			logger.L().Infof("待同步股票数: %d", len(allStocks))

			finaSynced := 0
			failedCount := 0
			for i, stock := range allStocks {
				if i%100 == 0 {
					logger.L().Infof("进度: %d/%d (已同步%d条)", i, len(allStocks), finaSynced)
				}

				// 逐报告期获取每只股票的财务指标
				for _, period := range periods {
					indicators, err := tsClient.FinaIndicator(stock.TsCode, period)
					if err != nil {
						failedCount++
						continue
					}
					if len(indicators) == 0 {
						continue
					}
					if err := finaRepo.BatchInsert(indicators); err != nil {
						logger.L().Errorf("存储 %s 财务指标失败: %v", stock.TsCode, err)
						continue
					}
					finaSynced += len(indicators)
				}
			}
			logger.L().Infof("财务指标同步完成, 共 %d 条, 失败 %d 次", finaSynced, failedCount)
		}
	}

	logger.L().Info("数据同步全部完成!")
}

var watchCodeCache map[string]bool
var watchCodeCacheDirty = true

// getWatchCodeCache 获取关注股票代码集合(带缓存)
func getWatchCodeCache(cfg *config.Config, stockRepo *store.StockRepo, portfolioRepo *store.PortfolioRepo) map[string]bool {
	if !watchCodeCacheDirty {
		return watchCodeCache
	}
	watchCodeCache = buildWatchCodeSet(cfg, stockRepo, portfolioRepo)
	watchCodeCacheDirty = false
	return watchCodeCache
}

// buildWatchCodeSet 构建关注的股票代码集合
// 包含: 股票池(bluechip + tech) + 持仓 + watchlist 配置
func buildWatchCodeSet(cfg *config.Config, stockRepo *store.StockRepo, portfolioRepo *store.PortfolioRepo) map[string]bool {
	codeSet := make(map[string]bool)

	// 1. 股票池
	if cfg.Universe.Bluechip != "" {
		for _, code := range strings.Split(cfg.Universe.Bluechip, ",") {
			code = strings.TrimSpace(code)
			if code != "" {
				codeSet[code] = true
			}
		}
	}
	if cfg.Universe.Tech != "" {
		for _, code := range strings.Split(cfg.Universe.Tech, ",") {
			code = strings.TrimSpace(code)
			if code != "" {
				codeSet[code] = true
			}
		}
	}

	// 2. 配置中的 watchlist
	for _, code := range cfg.Dataloader.Watchlist {
		code = strings.TrimSpace(code)
		if code != "" {
			codeSet[code] = true
		}
	}

	// 3. 当前持仓
	if portfolioRepo != nil {
		positions, err := portfolioRepo.GetAllPositions()
		if err == nil {
			for _, p := range positions {
				codeSet[p.TsCode] = true
			}
		}
	}

	return codeSet
}

// cleanupUnusedData 清理不在关注列表中的股票数据
// 返回删除的日线数、涨跌停数、基本面数
func cleanupUnusedData(db *sqlx.DB, watchCodes map[string]bool) (int, int, int, error) {
	// 构建 IN 查询的占位符
	codes := make([]string, 0, len(watchCodes))
	for code := range watchCodes {
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return 0, 0, 0, fmt.Errorf("关注列表为空, 拒绝清理全部数据")
	}

	// 构建 NOT IN 的参数
	placeholders := make([]string, len(codes))
	args := make([]interface{}, len(codes))
	for i, code := range codes {
		placeholders[i] = "?"
		args[i] = code
	}
	notIn := strings.Join(placeholders, ",")

	// 删除日线
	result, err := db.Exec(fmt.Sprintf("DELETE FROM daily_bar WHERE ts_code NOT IN (%s)", notIn), args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("清理日线失败: %w", err)
	}
	deletedBars, _ := result.RowsAffected()

	// 删除涨跌停
	result, err = db.Exec(fmt.Sprintf("DELETE FROM stk_limit WHERE ts_code NOT IN (%s)", notIn), args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("清理涨跌停失败: %w", err)
	}
	deletedLimits, _ := result.RowsAffected()

	// 删除每日基本面
	result, err = db.Exec(fmt.Sprintf("DELETE FROM daily_basic WHERE ts_code NOT IN (%s)", notIn), args...)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("清理基本面失败: %w", err)
	}
	deletedBasics, _ := result.RowsAffected()

	return int(deletedBars), int(deletedLimits), int(deletedBasics), nil
}

// genReportPeriods 生成 [startDate, endDate] 区间内的报告期列表(降序, 最近的在前)
// A股报告期: 0331(一季报) 0630(半年报) 0930(三季报) 1231(年报)
// startDate/endDate 格式: YYYYMMDD
func genReportPeriods(startDate, endDate string) []string {
	// 报告期的月日后缀, 按降序排列以便最新的报告期优先同步
	quarterSuffixes := []string{"1231", "0930", "0630", "0331"}

	var periods []string
	start, err1 := time.Parse("20060102", startDate)
	end, err2 := time.Parse("20060102", endDate)
	if err1 != nil || err2 != nil {
		return periods
	}

	for y := end.Year(); y >= start.Year(); y-- {
		for _, suffix := range quarterSuffixes {
			period := fmt.Sprintf("%d%s", y, suffix)
			if period >= startDate && period <= endDate {
				periods = append(periods, period)
			}
		}
	}
	return periods
}
