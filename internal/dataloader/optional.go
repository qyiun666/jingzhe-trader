package dataloader

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// syncOptional 同步可选数据 (新闻/资金流/龙虎榜/财务指标)
// 返回各子任务的失败原因: 这些是辅助数据源, 失败不该阻断核心行情同步,
// 但必须让调用方可见 —— 此前只记日志不上报, 导致四张表长期 0 行而任务仍记 success
func (l *Loader) syncOptional(opts Options, tradeCals []model.TradeCal) []string {
	var failures []string
	report := func(name string, err error) {
		if err != nil {
			logger.L().Errorf("同步%s失败: %v", name, err)
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
		}
	}
	if opts.SyncNews {
		report("新闻快讯", l.syncNews(opts.StartDate, opts.EndDate))
	}
	if opts.SyncMoneyFlow {
		report("个股资金流向", l.syncMoneyFlow(tradeCals))
	}
	if opts.SyncTopList {
		report("龙虎榜", l.syncTopList(tradeCals))
	}
	if opts.SyncFina {
		report("财务指标", l.syncFina(opts.StartDate, opts.EndDate))
	}
	return failures
}

// syncNews 同步新闻快讯
func (l *Loader) syncNews(startDate, endDate string) error {
	logger.L().Info("=== 同步新闻快讯 ===")
	newsList, err := l.ts.MajorNews(startDate, endDate, "")
	if err != nil {
		return fmt.Errorf("获取失败: %w", err)
	}
	if err := store.NewNewsRepo(l.db).BatchInsert(newsList); err != nil {
		return fmt.Errorf("入库失败: %w", err)
	}
	logger.L().Infof("新闻快讯同步完成: %d 条", len(newsList))
	return nil
}

// maxDateRepo 支持查询最大交易日的 repo (增量同步判断用)
type maxDateRepo interface {
	GetMaxTradeDate() (string, error)
}

// syncByTradeDay 按交易日增量同步 (交易日间并行拉取): 拉取 → 入库, 跳过已同步日期
// 返回 (成功同步的交易日数, 尝试同步的交易日数)
func syncByTradeDay[T any](repo maxDateRepo, tradeCals []model.TradeCal, name string,
	fetch func(string) ([]T, error), store func([]T) error) (int, int) {
	logger.L().Infof("=== 同步%s ===", name)
	lastDate, _ := repo.GetMaxTradeDate()

	var mu sync.Mutex
	synced := 0
	attempts := 0
	var wg sync.WaitGroup
	sem := make(chan struct{}, dataloaderConcurrency)
	for _, cal := range tradeCals {
		if lastDate != "" && cal.CalDate <= lastDate {
			continue
		}
		calDate := cal.CalDate
		attempts++
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			items, err := fetch(calDate)
			if err != nil {
				logger.L().Errorf("获取 %s %s失败: %v", calDate, name, err)
				return
			}
			if len(items) == 0 {
				mu.Lock()
				synced++ // 当日确无数据 (如无上榜), 属成功
				mu.Unlock()
				return
			}
			if err := store(items); err != nil {
				logger.L().Errorf("存储 %s %s失败: %v", calDate, name, err)
				return
			}
			mu.Lock()
			synced++
			mu.Unlock()
		}()
	}
	wg.Wait()
	logger.L().Infof("%s同步完成, 共 %d/%d 个交易日", name, synced, attempts)
	return synced, attempts
}

// daySyncError 判定"按交易日同步"是否整体失败
// 只在有尝试且无一成功时报错: 个别交易日失败属常态 (停牌/无上榜), 天天告警会淹没真问题
// 数据源名前缀由调用方 syncOptional 统一附加
func daySyncError(synced, attempts int) error {
	if attempts > 0 && synced == 0 {
		return fmt.Errorf("%d 个交易日全部失败 (接口不可用或权限不足)", attempts)
	}
	return nil
}

// syncMoneyFlow 同步个股资金流向 (按交易日增量)
func (l *Loader) syncMoneyFlow(tradeCals []model.TradeCal) error {
	repo := store.NewMoneyFlowRepo(l.db)
	synced, attempts := syncByTradeDay(repo, tradeCals, "个股资金流向", l.ts.MoneyFlow, repo.BatchInsert)
	return daySyncError(synced, attempts)
}

// syncTopList 同步龙虎榜 (按交易日增量)
func (l *Loader) syncTopList(tradeCals []model.TradeCal) error {
	repo := store.NewTopListRepo(l.db)
	synced, attempts := syncByTradeDay(repo, tradeCals, "龙虎榜", l.ts.TopList, repo.BatchInsert)
	return daySyncError(synced, attempts)
}

// syncFina 同步财务指标 (逐股票并行, 股票内各报告期串行)
// Tushare 500元档 fina_indicator 必须传 ts_code, 不能按报告期批量获取
func (l *Loader) syncFina(startDate, endDate string) error {
	logger.L().Info("=== 同步财务指标 ===")
	finaRepo := store.NewFinaRepo(l.db)

	allStocks, err := l.stockRepo.GetAll()
	if err != nil {
		return fmt.Errorf("获取股票列表失败: %w", err)
	}
	if len(allStocks) == 0 {
		return fmt.Errorf("股票列表为空, 无法按股票拉取财务指标")
	}

	periods := genReportPeriods(startDate, endDate)
	if len(periods) == 0 {
		// 增量窗口通常落在两个季度末之间, 无报告期是正常情形而非故障;
		// 当失败返回会让 data_update 每个交易日都误报
		logger.L().Infof("区间 %s~%s 内无报告期, 跳过财务指标同步", startDate, endDate)
		return nil
	}
	logger.L().Infof("待同步报告期: %v, 股票数: %d", periods, len(allStocks))

	var mu sync.Mutex
	finaSynced := 0
	failedCount := 0
	processed := 0
	total := len(allStocks)

	var wg sync.WaitGroup
	sem := make(chan struct{}, dataloaderConcurrency)
	for _, stock := range allStocks {
		wg.Add(1)
		go func(stock model.Stock) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			for _, period := range periods {
				indicators, err := l.ts.FinaIndicator(stock.TsCode, period)
				if err != nil {
					mu.Lock()
					failedCount++
					mu.Unlock()
					continue
				}
				if len(indicators) == 0 {
					continue
				}
				if err := finaRepo.BatchInsert(indicators); err != nil {
					logger.L().Errorf("存储 %s 财务指标失败: %v", stock.TsCode, err)
					continue
				}
				mu.Lock()
				finaSynced += len(indicators)
				mu.Unlock()
			}

			mu.Lock()
			processed++
			if processed%100 == 0 {
				logger.L().Infof("进度: %d/%d (已同步%d条)", processed, total, finaSynced)
			}
			mu.Unlock()
		}(stock)
	}
	wg.Wait()
	logger.L().Infof("财务指标同步完成, 共 %d 条, 失败 %d 次", finaSynced, failedCount)

	if finaSynced == 0 && failedCount > 0 {
		return fmt.Errorf("%d 只股票 × %d 个报告期全部拉取失败 (接口不可用或权限不足)", len(allStocks), len(periods))
	}
	return nil
}

// Cleanup 清理不在关注列表中的股票数据 (危险操作)
// 需同时满足: 配置 dataloader.enable_cleanup=true 且调用方显式确认
func (l *Loader) Cleanup(confirmed bool) error {
	if !l.cfg.Dataloader.EnableCleanup {
		return fmt.Errorf("清理未启用: 请在配置中设置 dataloader.enable_cleanup=true")
	}
	if !confirmed {
		return fmt.Errorf("清理未确认: CLI 需加 --confirm-cleanup 参数")
	}

	watchCodes := l.watchCodes()
	if len(watchCodes) == 0 {
		return fmt.Errorf("关注列表为空, 拒绝清理全部数据")
	}
	logger.L().Warnf("=== 清理多余股票数据: 保留 %d 只关注股票, 其余物理删除 ===", len(watchCodes))

	deletedBars, deletedLimits, deletedBasics, err := l.deleteUnwatched(watchCodes)
	if err != nil {
		return fmt.Errorf("清理数据失败: %w", err)
	}
	logger.L().Infof("清理完成: 删除 %d 条日线, %d 条涨跌停, %d 条基本面",
		deletedBars, deletedLimits, deletedBasics)

	if _, err := l.db.Exec("VACUUM"); err != nil {
		logger.L().Warnf("VACUUM 失败: %v", err)
	} else {
		logger.L().Info("VACUUM 完成, 空间已回收")
	}
	return nil
}

// deleteUnwatched 删除不在关注列表中的行情数据
func (l *Loader) deleteUnwatched(watchCodes map[string]bool) (int64, int64, int64, error) {
	codes := make([]interface{}, 0, len(watchCodes))
	placeholders := make([]string, 0, len(watchCodes))
	for code := range watchCodes {
		codes = append(codes, code)
		placeholders = append(placeholders, "?")
	}
	notIn := strings.Join(placeholders, ",")

	deleteFrom := func(table string) (int64, error) {
		result, err := l.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE ts_code NOT IN (%s)", table, notIn), codes...)
		if err != nil {
			return 0, fmt.Errorf("清理 %s 失败: %w", table, err)
		}
		return result.RowsAffected()
	}

	deletedBars, err := deleteFrom("daily_bar")
	if err != nil {
		return 0, 0, 0, err
	}
	deletedLimits, err := deleteFrom("stk_limit")
	if err != nil {
		return deletedBars, 0, 0, err
	}
	deletedBasics, err := deleteFrom("daily_basic")
	if err != nil {
		return deletedBars, deletedLimits, 0, err
	}
	return deletedBars, deletedLimits, deletedBasics, nil
}

// genReportPeriods 生成 [startDate, endDate] 区间内的报告期列表(降序, 最近的在前)
// A股报告期: 0331(一季报) 0630(半年报) 0930(三季报) 1231(年报)
func genReportPeriods(startDate, endDate string) []string {
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
