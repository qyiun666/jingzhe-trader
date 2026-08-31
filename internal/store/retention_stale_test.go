package store

import (
	"path/filepath"
	"testing"
	"time"
)

// 陈旧股票清理必须保留最近工作窗口内的全市场行情
// 背景: 选股器的趋势过滤按日读全市场收盘价算 MA5, 此前把非活跃股票的行情一并删光,
// 实测近5日收盘价覆盖从 2137 只跌到 22 只, 趋势过滤静默失效而候选数仍是 20, 看不出退化
func TestCleanStaleStocksKeepsRecentWindow(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	old := time.Now().AddDate(0, 0, -StaleKeepRecentDays-20).Format("20060102")
	recent := time.Now().AddDate(0, 0, -3).Format("20060102")

	for _, code := range []string{"STALE.SH", "KEEP.SH"} {
		db.MustExec(`INSERT INTO daily_bar (ts_code, trade_date, close) VALUES (?, ?, 10)`, code, old)
		db.MustExec(`INSERT INTO daily_bar (ts_code, trade_date, close) VALUES (?, ?, 10)`, code, recent)
		db.MustExec(`INSERT INTO daily_basic (ts_code, trade_date) VALUES (?, ?)`, code, old)
		db.MustExec(`INSERT INTO daily_basic (ts_code, trade_date) VALUES (?, ?)`, code, recent)
		db.MustExec(`INSERT INTO stk_limit (ts_code, trade_date) VALUES (?, ?)`, code, old)
		db.MustExec(`INSERT INTO stk_limit (ts_code, trade_date) VALUES (?, ?)`, code, recent)
	}

	if err := CleanStaleStocks(db, []string{"KEEP.SH"}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	count := func(table, code, date string) int {
		var n int
		if err := db.Get(&n, `SELECT COUNT(1) FROM `+table+` WHERE ts_code = ? AND trade_date = ?`, code, date); err != nil {
			t.Fatalf("统计 %s 失败: %v", table, err)
		}
		return n
	}
	for _, table := range []string{"daily_bar", "daily_basic", "stk_limit"} {
		if got := count(table, "STALE.SH", old); got != 0 {
			t.Errorf("%s: 窗口外的陈旧股票数据应被删除, got %d 行", table, got)
		}
		if got := count(table, "STALE.SH", recent); got != 1 {
			t.Errorf("%s: 窗口内的全市场数据必须保留 (选股器按日读全市场收盘价), got %d 行", table, got)
		}
		if got := count(table, "KEEP.SH", old); got != 1 {
			t.Errorf("%s: 活跃股票的历史数据不应被删除, got %d 行", table, got)
		}
	}
}
