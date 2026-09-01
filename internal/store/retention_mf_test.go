package store

import (
	"path/filepath"
	"testing"
	"time"
)

// moneyflow 走自己的保留窗口, 不再跟 bar_years
// 背景: 该表每个交易日落全市场约 5652 行, 与 bar_years=3 共用窗口实测攒到 372 万行 354MB
// (占整库 87%), 而唯一读取方只取单票近 14 天; 日线同期只 11 万行, 因为它只留候选股票。
func TestRetentionMoneyFlowUsesOwnWindow(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	insert := func(days int) string {
		d := time.Now().AddDate(0, 0, -days).Format("20060102")
		db.MustExec(`INSERT INTO moneyflow (ts_code, trade_date, net_mf_amount) VALUES ('000001.SZ', ?, 1)`, d)
		db.MustExec(`INSERT INTO daily_bar (ts_code, trade_date, close) VALUES ('000001.SZ', ?, 10)`, d)
		return d
	}
	insideMf := insert(10)
	outsideMf := insert(90)

	if err := cleanMarketData(db, RetentionPolicy{BarYears: 3, MfDays: 60}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	count := func(table, date string) int {
		var n int
		if err := db.Get(&n, `SELECT COUNT(1) FROM `+table+` WHERE trade_date = ?`, date); err != nil {
			t.Fatalf("统计 %s 失败: %v", table, err)
		}
		return n
	}
	if count("moneyflow", insideMf) != 1 {
		t.Errorf("窗口内的资金流向应保留, got %d 行", count("moneyflow", insideMf))
	}
	if count("moneyflow", outsideMf) != 0 {
		t.Errorf("超出 mf_days 的资金流向应删除, got %d 行", count("moneyflow", outsideMf))
	}
	if count("daily_bar", outsideMf) != 1 {
		t.Errorf("日线仍按 bar_years 保留, got %d 行", count("daily_bar", outsideMf))
	}
}

// MfDays=0 表示不清理, 且此时 bar_years 也不得连带裁掉 moneyflow
func TestRetentionMoneyFlowZeroWindowKeepsRows(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	old := time.Now().AddDate(0, 0, -400).Format("20060102")
	db.MustExec(`INSERT INTO moneyflow (ts_code, trade_date, net_mf_amount) VALUES ('000001.SZ', ?, 1)`, old)
	db.MustExec(`INSERT INTO daily_bar (ts_code, trade_date, close) VALUES ('000001.SZ', ?, 10)`, old)

	if err := cleanMarketData(db, RetentionPolicy{BarYears: 1, MfDays: 0}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}

	var mfRows int
	if err := db.Get(&mfRows, `SELECT COUNT(1) FROM moneyflow`); err != nil {
		t.Fatalf("统计 moneyflow 失败: %v", err)
	}
	if mfRows != 1 {
		t.Errorf("MfDays=0 时资金流向不该被清理, got %d 行", mfRows)
	}
	var barRows int
	if err := db.Get(&barRows, `SELECT COUNT(1) FROM daily_bar`); err != nil {
		t.Fatalf("统计 daily_bar 失败: %v", err)
	}
	if barRows != 0 {
		t.Errorf("超出 bar_years 的日线应被清理, got %d 行", barRows)
	}
}

// 历史库启动后不该再有按 trade_date 的索引: 唯一的单日查询路径已删除, 留着每次同步多写 67MB
func TestMoneyFlowDateIndexDropped(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	db.MustExec(`CREATE INDEX IF NOT EXISTS idx_moneyflow_date ON moneyflow(trade_date)`)
	if err := migrate(db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	var n int
	if err := db.Get(&n, `SELECT COUNT(1) FROM sqlite_master WHERE type = 'index' AND name = 'idx_moneyflow_date'`); err != nil {
		t.Fatalf("查询索引失败: %v", err)
	}
	if n != 0 {
		t.Error("idx_moneyflow_date 应在迁移时被删除")
	}
}
