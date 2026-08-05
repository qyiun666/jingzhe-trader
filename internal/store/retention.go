package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/pkg/logger"
)

// RetentionPolicy 数据保留策略 (来自 retention 配置段)
type RetentionPolicy struct {
	BarYears     int    // 行情数据保留年数
	NewsDays     int    // 新闻保留天数
	PlanDays     int    // 交易计划/任务记录保留天数
	BacktestRuns int    // 保留最近N个回测run (live_* 前缀永久保留)
	LogDays      int    // 日志文件保留天数
	ReportFiles  int    // 保留最近N个报告文件
	LogDir       string // 日志目录 (空则跳过)
	ReportDir    string // 报告目录 (空则跳过)
}

// RunRetention 执行数据保留清理
// fullClean=false 时仅做文件清理 (非交易日跳过数据库大项)
// 所有删除走参数化SQL+事务, 失败只返回错误由调用方告警, 不影响交易任务
func RunRetention(db *sqlx.DB, p RetentionPolicy, fullClean bool) error {
	logDBSize(db, "清理前")

	var errs []string
	if fullClean {
		if err := cleanMarketData(db, p); err != nil {
			errs = append(errs, err.Error())
		}
		if err := cleanBacktestRuns(db, p.BacktestRuns); err != nil {
			errs = append(errs, err.Error())
		}
		if err := cleanPlansAndJobs(db, p.PlanDays); err != nil {
			errs = append(errs, err.Error())
		}
	}
	cleanOldFiles(p.LogDir, p.LogDays)
	keepNewestFiles(p.ReportDir, p.ReportFiles)

	// SQLite 瘦身: 每日 checkpoint 防 -wal 无限增长; 周日回收空间
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		errs = append(errs, fmt.Sprintf("wal_checkpoint失败: %v", err))
	}
	if time.Now().Weekday() == time.Sunday {
		if _, err := db.Exec(`PRAGMA incremental_vacuum`); err != nil {
			errs = append(errs, fmt.Sprintf("incremental_vacuum失败: %v", err))
		}
	}

	logDBSize(db, "清理后")
	if len(errs) > 0 {
		return fmt.Errorf("数据清理部分失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

// cleanMarketData 清理过期行情与新闻数据
func cleanMarketData(db *sqlx.DB, p RetentionPolicy) error {
	if p.BarYears > 0 {
		cutoff := time.Now().AddDate(-p.BarYears, 0, 0).Format("20060102")
		for _, table := range []string{"daily_bar", "daily_basic", "stk_limit", "moneyflow"} {
			if err := deleteRows(db, table,
				fmt.Sprintf(`DELETE FROM %s WHERE trade_date < ?`, table), cutoff); err != nil {
				return err
			}
		}
	}
	if p.NewsDays > 0 {
		// news.datetime 格式 "2006-01-02 15:04:05"
		cutoff := time.Now().AddDate(0, 0, -p.NewsDays).Format("2006-01-02")
		if err := deleteRows(db, "news", `DELETE FROM news WHERE datetime < ?`, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// CleanStaleStocks 清理不在活跃股票池中的陈旧行情数据
// keepCodes: 需要保留的股票代码集合 (选股结果 + 持仓 + watchlist + universe)
// 删除这些股票的 daily_bar, daily_basic, stk_limit 数据, 释放空间
func CleanStaleStocks(db *sqlx.DB, keepCodes []string) error {
	if len(keepCodes) == 0 {
		return nil // 空集合不清理, 防止误删全表
	}

	// 查询库中所有不同的 ts_code
	var allCodes []string
	if err := db.Select(&allCodes, `SELECT DISTINCT ts_code FROM daily_bar`); err != nil {
		return fmt.Errorf("查询库内股票代码失败: %w", err)
	}

	// 找出需要清理的代码 (不在保留集合中)
	keepSet := make(map[string]bool, len(keepCodes))
	for _, code := range keepCodes {
		keepSet[code] = true
	}
	var staleCodes []string
	for _, code := range allCodes {
		if !keepSet[code] {
			staleCodes = append(staleCodes, code)
		}
	}

	if len(staleCodes) == 0 {
		logger.L().Info("无陈旧股票数据需要清理")
		return nil
	}

	logger.L().Infow("清理陈旧股票数据", "stale_count", len(staleCodes), "keep_count", len(keepCodes))

	// 分批清理 (每批 100 个, 避免 SQL IN 子句过长)
	batchSize := 100
	var totalDeleted int64
	for i := 0; i < len(staleCodes); i += batchSize {
		end := i + batchSize
		if end > len(staleCodes) {
			end = len(staleCodes)
		}
		batch := staleCodes[i:end]

		// 构造 IN (?, ?, ...) 占位符
		placeholders := strings.Repeat("?,", len(batch))
		placeholders = placeholders[:len(placeholders)-1] // 去掉末尾逗号
		args := make([]interface{}, len(batch))
		for j, code := range batch {
			args[j] = code
		}

		for _, table := range []string{"daily_bar", "daily_basic", "stk_limit"} {
			query := fmt.Sprintf(`DELETE FROM %s WHERE ts_code IN (%s)`, table, placeholders)
			res, err := db.Exec(query, args...)
			if err != nil {
				logger.L().Warnw("清理陈旧数据失败", "table", table, "err", err)
				continue
			}
			n, _ := res.RowsAffected()
			totalDeleted += n
		}
	}

	logger.L().Infow("陈旧股票数据清理完成", "deleted_rows", totalDeleted, "stale_stocks", len(staleCodes))
	return nil
}

// GetActiveStockCodes 获取活跃股票代码集合 (选股结果 + 持仓 + watchlist)
// 供 CleanStaleStocks 使用, 确保不误删在用股票的数据
func GetActiveStockCodes(db *sqlx.DB, watchlist []string, universeCodes []string) []string {
	codeSet := make(map[string]bool)

	// 选股结果
	var screenCodes []string
	if err := db.Select(&screenCodes, "SELECT DISTINCT ts_code FROM screen_result"); err == nil {
		for _, code := range screenCodes {
			codeSet[code] = true
		}
	}

	// 持仓
	var posCodes []string
	if err := db.Select(&posCodes, "SELECT DISTINCT ts_code FROM portfolio WHERE total_qty > 0"); err == nil {
		for _, code := range posCodes {
			codeSet[code] = true
		}
	}

	// watchlist (指数等)
	for _, code := range watchlist {
		if code = strings.TrimSpace(code); code != "" {
			codeSet[code] = true
		}
	}

	// universe (如果手动配置了)
	for _, code := range universeCodes {
		if code = strings.TrimSpace(code); code != "" {
			codeSet[code] = true
		}
	}

	var result []string
	for code := range codeSet {
		result = append(result, code)
	}
	return result
}

// cleanBacktestRuns 只保留最近N个回测run的成交与快照
// 实盘 runID (live_* 前缀) 永久保留 (审计需要)
func cleanBacktestRuns(db *sqlx.DB, keepRuns int) error {
	if keepRuns <= 0 {
		return nil
	}
	var runIDs []string
	err := db.Select(&runIDs, `SELECT run_id FROM trades
		WHERE run_id NOT LIKE 'live_%' AND run_id != ''
		GROUP BY run_id ORDER BY MAX(id) DESC`)
	if err != nil {
		return fmt.Errorf("查询回测run列表失败: %w", err)
	}
	if len(runIDs) <= keepRuns {
		return nil
	}

	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("开启清理事务失败: %w", err)
	}
	defer tx.Rollback()

	var total int64
	for _, runID := range runIDs[keepRuns:] {
		for _, table := range []string{"trades", "account_snapshot", "position_snapshot", "orders"} {
			res, err := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE run_id = ?`, table), runID)
			if err != nil {
				return fmt.Errorf("清理 %s(run=%s) 失败: %w", table, runID, err)
			}
			n, _ := res.RowsAffected()
			total += n
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交清理事务失败: %w", err)
	}
	logger.L().Infow("回测run清理完成", "deleted_runs", len(runIDs)-keepRuns, "deleted_rows", total)
	return nil
}

// cleanPlansAndJobs 清理过期交易计划与任务记录
func cleanPlansAndJobs(db *sqlx.DB, planDays int) error {
	if planDays <= 0 {
		return nil
	}
	cutoff := time.Now().AddDate(0, 0, -planDays).Format("20060102")
	if err := deleteRows(db, "trade_plan", `DELETE FROM trade_plan WHERE trade_date < ?`, cutoff); err != nil {
		return err
	}
	jobCutoff := time.Now().AddDate(0, 0, -planDays).Format("2006-01-02")
	return deleteRows(db, "job_run", `DELETE FROM job_run WHERE started_at < ?`, jobCutoff)
}

// deleteRows 参数化删除并记录行数
func deleteRows(db *sqlx.DB, table, query string, args ...interface{}) error {
	res, err := db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("清理 %s 失败: %w", table, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		logger.L().Infow("数据清理", "table", table, "deleted_rows", n)
	}
	return nil
}

// cleanOldFiles 删除目录下超过 days 天的文件 (日志清理)
func cleanOldFiles(dir string, days int) {
	if dir == "" || days <= 0 {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // 目录不存在则跳过
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Remove(path); err != nil {
			logger.L().Warnw("删除过期文件失败", "path", path, "err", err)
		} else {
			logger.L().Infow("删除过期文件", "path", path)
		}
	}
}

// keepNewestFiles 只保留目录下最新的 keep 个文件 (报告清理)
func keepNewestFiles(dir string, keep int) {
	if dir == "" || keep <= 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type fileInfo struct {
		path    string
		modTime time.Time
	}
	var files []fileInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, fileInfo{filepath.Join(dir, e.Name()), info.ModTime()})
	}
	if len(files) <= keep {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	for _, f := range files[keep:] {
		if err := os.Remove(f.path); err != nil {
			logger.L().Warnw("删除旧报告失败", "path", f.path, "err", err)
		} else {
			logger.L().Infow("删除旧报告", "path", f.path)
		}
	}
}

// logDBSize 打印主要表行数与db文件大小, 便于观察清理效果
func logDBSize(db *sqlx.DB, stage string) {
	counts := map[string]int{}
	for _, table := range []string{"daily_bar", "news", "trades", "account_snapshot", "trade_plan", "job_run"} {
		var n int
		if err := db.Get(&n, fmt.Sprintf(`SELECT COUNT(1) FROM %s`, table)); err == nil {
			counts[table] = n
		}
	}
	var dbPath string
	_ = db.Get(&dbPath, `SELECT file FROM pragma_database_list WHERE name='main'`)
	var size int64
	if dbPath != "" {
		if info, err := os.Stat(dbPath); err == nil {
			size = info.Size()
		}
	}
	logger.L().Infow("数据库规模("+stage+")", "rows", counts, "db_bytes", size)
}
