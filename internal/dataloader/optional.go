package dataloader

import (
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// syncOptional 同步可选数据 (新股/新闻/资金流/龙虎榜/财务指标)
func (l *Loader) syncOptional(opts Options, tradeCals []model.TradeCal) {
	if opts.SyncNewShare {
		l.syncNewShares(opts.StartDate, opts.EndDate)
	}
	if opts.SyncNews {
		l.syncNews(opts.StartDate, opts.EndDate)
	}
	if opts.SyncMoneyFlow {
		l.syncMoneyFlow(tradeCals)
	}
	if opts.SyncTopList {
		l.syncTopList(tradeCals)
	}
	if opts.SyncFina {
		l.syncFina(opts.StartDate, opts.EndDate)
	}
}

// syncNewShares 同步新股申购数据
func (l *Loader) syncNewShares(startDate, endDate string) {
	logger.L().Info("=== 同步新股申购数据 ===")
	newShares, err := l.ts.NewShare(startDate, endDate)
	if err != nil {
		logger.L().Errorf("获取新股申购数据失败: %v", err)
		return
	}
	nsRepo := store.NewNewShareRepo(l.db)
	if err := nsRepo.BatchInsert(newShares); err != nil {
		logger.L().Errorf("存储新股申购数据失败: %v", err)
		return
	}
	logger.L().Infof("新股申购数据同步完成: %d 条", len(newShares))
}

// syncNews 同步新闻快讯
func (l *Loader) syncNews(startDate, endDate string) {
	logger.L().Info("=== 同步新闻快讯 ===")
	newsList, err := l.ts.MajorNews(startDate, endDate, "")
	if err != nil {
		logger.L().Errorf("获取新闻快讯失败: %v", err)
		return
	}
	newsRepo := store.NewNewsRepo(l.db)
	if err := newsRepo.BatchInsert(newsList); err != nil {
		logger.L().Errorf("存储新闻快讯失败: %v", err)
		return
	}
	logger.L().Infof("新闻快讯同步完成: %d 条", len(newsList))
}

// maxDateRepo 支持查询最大交易日的 repo (增量同步判断用)
type maxDateRepo interface {
	GetMaxTradeDate() (string, error)
}

// syncByTradeDay 按交易日增量同步: 拉取 → 入库, 跳过已同步日期 (syncMoneyFlow/syncTopList 共用)
// fetch: 拉取某交易日数据; store: 批量入库; 返回同步的交易日数
func syncByTradeDay[T any](repo maxDateRepo, tradeCals []model.TradeCal, name string,
	fetch func(string) ([]T, error), store func([]T) error) int {
	logger.L().Infof("=== 同步%s ===", name)
	lastDate, _ := repo.GetMaxTradeDate()
	synced := 0
	for _, cal := range tradeCals {
		if lastDate != "" && cal.CalDate <= lastDate {
			continue
		}
		items, err := fetch(cal.CalDate)
		if err != nil {
			logger.L().Errorf("获取 %s %s失败: %v", cal.CalDate, name, err)
			continue
		}
		if len(items) == 0 {
			continue
		}
		if err := store(items); err != nil {
			logger.L().Errorf("存储 %s %s失败: %v", cal.CalDate, name, err)
			continue
		}
		synced++
	}
	logger.L().Infof("%s同步完成, 共 %d 个交易日", name, synced)
	return synced
}

// syncMoneyFlow 同步个股资金流向 (按交易日增量)
func (l *Loader) syncMoneyFlow(tradeCals []model.TradeCal) {
	repo := store.NewMoneyFlowRepo(l.db)
	syncByTradeDay(repo, tradeCals, "个股资金流向", l.ts.MoneyFlow, repo.BatchInsert)
}

// syncTopList 同步龙虎榜 (按交易日增量)
func (l *Loader) syncTopList(tradeCals []model.TradeCal) {
	repo := store.NewTopListRepo(l.db)
	syncByTradeDay(repo, tradeCals, "龙虎榜", l.ts.TopList, repo.BatchInsert)
}

// syncFina 同步财务指标 (逐股票逐报告期获取)
// Tushare 500元档 fina_indicator 必须传 ts_code, 不能按报告期批量获取
func (l *Loader) syncFina(startDate, endDate string) {
	logger.L().Info("=== 同步财务指标 ===")
	finaRepo := store.NewFinaRepo(l.db)

	allStocks, err := l.stockRepo.GetAll()
	if err != nil || len(allStocks) == 0 {
		logger.L().Errorf("获取股票列表失败: %v", err)
		return
	}

	periods := genReportPeriods(startDate, endDate)
	logger.L().Infof("待同步报告期: %v, 股票数: %d", periods, len(allStocks))

	finaSynced := 0
	failedCount := 0
	for i, stock := range allStocks {
		if i%100 == 0 {
			logger.L().Infof("进度: %d/%d (已同步%d条)", i, len(allStocks), finaSynced)
		}
		for _, period := range periods {
			indicators, err := l.ts.FinaIndicator(stock.TsCode, period)
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
