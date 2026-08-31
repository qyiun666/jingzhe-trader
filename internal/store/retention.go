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
	ActionDays   int    // 动作日志(action_log)保留天数 (每任务每笔都写, 增长最快)
	AlertDays    int    // 告警(agent_alert)保留天数
	ScreenDays   int    // 选股结果(screen_result)保留天数
	DebateDays   int    // 辩论/复盘记录保留天数
	FinaQuarters int    // 财务指标保留最近N个报告期
	BacktestRuns int    // 保留最近N个回测run (live_* 前缀永久保留)
	LogDays      int    // 日志文件保留天数
	ReportFiles  int    // 保留最近N个报告文件
	LogDir       string // 日志目录 (空则跳过)
	ReportDir    string // 报告目录 (空则跳过)
}

// alertUnreadGraceDays 超期告警中"未读"的额外宽限期: 没人看过的告警不随批抹掉, 但也不是永久保留
const alertUnreadGraceDays = 7

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
		if err := cleanActivityTables(db, p); err != nil {
			errs = append(errs, err.Error())
		}
		if err := cleanFinaPeriods(db, p.FinaQuarters); err != nil {
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
	capStdoutLogs(p.LogDir)
	keepNewestFiles(p.ReportDir, p.ReportFiles)

	// SQLite 瘦身: 每日 checkpoint 防 -wal 无限增长; 周日回收空间
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		errs = append(errs, fmt.Sprintf("wal_checkpoint失败: %v", err))
	}
	if time.Now().Weekday() == time.Sunday {
		errs = append(errs, vacuumIfNeeded(db)...)
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
		for _, table := range []string{"daily_bar", "daily_basic", "stk_limit", "moneyflow", "top_list"} {
			if err := deleteRowsBatched(db, table,
				`trade_date < ?`, 0, cutoff); err != nil {
				return err
			}
		}
	}
	if p.NewsDays > 0 {
		// news.datetime 格式 "2006-01-02 15:04:05"
		cutoff := time.Now().AddDate(0, 0, -p.NewsDays).Format("2006-01-02")
		if err := deleteRowsBatched(db, "news", `datetime < ?`, 0, cutoff); err != nil {
			return err
		}
	}
	return nil
}

// cleanActivityTables 清理运行记录类表
// 这几张表此前完全没有保留策略, 其中 action_log 由每个调度任务每笔写入
// (盘中监控 5 分钟一次 ≈ 66 行/交易日), 不清理会成为库里增长最快的表
func cleanActivityTables(db *sqlx.DB, p RetentionPolicy) error {
	if p.ActionDays > 0 {
		if err := deleteRowsBatched(db, "action_log", `trade_date < ?`, 0,
			time.Now().AddDate(0, 0, -p.ActionDays).Format("20060102")); err != nil {
			return err
		}
	}
	if p.AlertDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -p.AlertDays).Format("20060102")
		grace := time.Now().AddDate(0, 0, -(p.AlertDays + alertUnreadGraceDays)).Format("20060102")
		// 未读告警多留 alertUnreadGraceDays 天: 没人看过的记录一旦删掉就彻底丢了线索
		if err := deleteRowsBatched(db, "agent_alert",
			`trade_date < ? AND NOT (status = 'unread' AND trade_date >= ?)`, 0, cutoff, grace); err != nil {
			return err
		}
	}
	if p.ScreenDays > 0 {
		if err := deleteRowsBatched(db, "screen_result", `trade_date < ?`, 0,
			time.Now().AddDate(0, 0, -p.ScreenDays).Format("20060102")); err != nil {
			return err
		}
	}
	if p.DebateDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -p.DebateDays).Format("20060102")
		for _, table := range []string{"agent_debate", "agent_debate_review"} {
			if err := deleteRowsBatched(db, table, `trade_date < ?`, 0, cutoff); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanFinaPeriods 财务指标只保留最近 keep 个报告期
// 该表按 end_date 组织 (季度末) 而非 trade_date, 不能用日期减法, 需按实际存在的报告期取序
func cleanFinaPeriods(db *sqlx.DB, keep int) error {
	if keep <= 0 {
		return nil
	}
	var periods []string
	if err := db.Select(&periods,
		`SELECT end_date FROM fina_indicator WHERE end_date <> '' GROUP BY end_date ORDER BY end_date DESC`); err != nil {
		return fmt.Errorf("查询报告期列表失败: %w", err)
	}
	if len(periods) <= keep {
		return nil
	}
	return deleteRowsBatched(db, "fina_indicator", `end_date < ?`, 0, periods[keep-1])
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
		for _, table := range []string{"trades", "account_snapshot"} {
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
	if err := deleteRowsBatched(db, "trade_plan", `trade_date < ?`, 0, cutoff); err != nil {
		return err
	}
	jobCutoff := time.Now().AddDate(0, 0, -planDays).Format("2006-01-02")
	return deleteRowsBatched(db, "job_run", `started_at < ?`, 0, jobCutoff)
}

// retentionBatchSize 分批删除的每批行数 (批间释放连接, 其他任务可插入)
const retentionBatchSize = 5000

// deleteRowsBatched 分批参数化删除 (每批独立提交, 批间释放 SQLite 连接)
// 背景: 进程内 SetMaxOpenConns(1), 大表 DELETE 的隐式长事务会阻塞后续所有任务落库
// (2026-08-26/27 曾因此出现 16:30 清理后调度整体卡死), 分批后其他任务可在批间隙获得连接
func deleteRowsBatched(db *sqlx.DB, table, where string, batchSize int, args ...interface{}) error {
	if batchSize <= 0 {
		batchSize = retentionBatchSize
	}
	var total int64
	for {
		query := fmt.Sprintf(`DELETE FROM %s WHERE rowid IN (SELECT rowid FROM %s WHERE %s LIMIT ?)`,
			table, table, where)
		res, err := db.Exec(query, append(append([]interface{}{}, args...), batchSize)...)
		if err != nil {
			return fmt.Errorf("清理 %s 失败: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batchSize) {
			break // 本批未满, 已删完
		}
	}
	if total > 0 {
		logger.L().Infow("数据清理(分批)", "table", table, "deleted_rows", total)
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
		// launchd-*.log 由进程管理器持有 fd 常驻, 删除会导致后续写入落到隐形 inode
		// (磁盘不回收), 体积控制交给 capStdoutLogs 截断, 这里跳过
		if isStdoutLog(e.Name()) {
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

// stdoutLogMaxBytes launchd stdout/stderr 单个日志体积上限 (超出截断清空)
const stdoutLogMaxBytes = 16 << 20 // 16MB

// isStdoutLog 判定是否为进程管理器重定向的标准输出日志 (launchd-*.log)
func isStdoutLog(name string) bool {
	return strings.HasPrefix(name, "launchd-") && strings.HasSuffix(name, ".log")
}

// capStdoutLogs 截断超体积的 launchd-*.log: launchd 以 O_APPEND 持有 fd,
// 截断到 0 后下次写入自动从文件头继续, 既不破坏 fd 又回收空间
func capStdoutLogs(dir string) {
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !isStdoutLog(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() <= stdoutLogMaxBytes {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if err := os.Truncate(path, 0); err != nil {
			logger.L().Warnw("截断标准输出日志失败", "path", path, "err", err)
		} else {
			logger.L().Infow("标准输出日志超上限已清空", "path", path, "size_bytes", info.Size())
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

// vacuumIfNeeded 按需回收 SQLite 空闲页 (删除数据后文件不收缩的问题)
//
// 背景: PRAGMA auto_vacuum 只对新建数据库生效。历史库 auto_vacuum=0 时,
// incremental_vacuum 是 no-op, 大量删除后文件空洞永不回收 (曾出现 1.4GB 文件仅 2% 有数据)。
//
// 策略:
//   - auto_vacuum=0 的旧库: 先持久化 INCREMENTAL 设置, 再跑真 VACUUM 一次性重建文件;
//     之后删除的页进入 freelist 可被自动复用, 周日只需 incremental_vacuum 增量回收
//   - auto_vacuum=INCREMENTAL 的新库: 直接 incremental_vacuum
//   - 空闲页占比 < 20% 时跳过 (VACUUM 重建成本高, 不值得)
func vacuumIfNeeded(db *sqlx.DB) []string {
	var errs []string
	var autoVacuum int
	if err := db.Get(&autoVacuum, "PRAGMA auto_vacuum"); err != nil {
		return []string{fmt.Sprintf("查询auto_vacuum失败: %v", err)}
	}

	var freelist, pageCount int64
	_ = db.Get(&freelist, "PRAGMA freelist_count")
	_ = db.Get(&pageCount, "PRAGMA page_count")
	freePct := 0.0
	if pageCount > 0 {
		freePct = float64(freelist) / float64(pageCount)
	}
	logger.L().Infow("SQLite空间检查", "auto_vacuum", autoVacuum, "freelist_pages", freelist, "page_count", pageCount, "free_pct", freePct)

	if freePct < 0.2 {
		logger.L().Info("空闲页占比低, 跳过空间回收")
		return nil
	}

	if autoVacuum == 0 {
		// 旧库: 设置 INCREMENTAL 需要 VACUUM 重建才持久化, 一次 VACUUM 同时完成回收+升级
		logger.L().Infow("旧库(auto_vacuum=0)空洞高, 执行 VACUUM 重建 (耗时取决于库大小)", "free_pct", freePct)
		if _, err := db.Exec("PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
			return []string{fmt.Sprintf("设置auto_vacuum失败: %v", err)}
		}
		if _, err := db.Exec("VACUUM"); err != nil {
			return []string{fmt.Sprintf("VACUUM失败(需磁盘空闲≈库大小): %v", err)}
		}
		logger.L().Info("VACUUM 完成, 空间已回收, auto_vacuum=INCREMENTAL 已持久化")
		return nil
	}

	if _, err := db.Exec("PRAGMA incremental_vacuum"); err != nil {
		errs = append(errs, fmt.Sprintf("incremental_vacuum失败: %v", err))
	}
	return errs
}

// logDBSize 打印主要表行数与db文件大小, 便于观察清理效果
func logDBSize(db *sqlx.DB, stage string) {
	counts := map[string]int{}
	for _, table := range []string{"daily_bar", "news", "trades", "account_snapshot", "trade_plan", "job_run",
		"action_log", "agent_alert", "screen_result", "top_list", "fina_indicator", "agent_debate"} {
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
