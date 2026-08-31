package store

import (
	"path/filepath"
	"testing"
)

// TestMigrateFreshSchema 新库应通过 migrate() 建出全部核心表, 且不残留旧表
func TestMigrateFreshSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")

	db, err := NewDB(path)
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	// 核心表应存在
	for _, tbl := range []string{
		"portfolio", "trade_plan", "action_log", "config_kv",
		"job_run", "trades", "account_snapshot", "agent_alert", "agent_debate",
	} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?`, tbl); err != nil {
			t.Fatalf("check %s: %v", tbl, err)
		}
		if n == 0 {
			t.Fatalf("表 %s 未创建", tbl)
		}
	}

	// 旧表不应残留
	for _, tbl := range []string{"portfolio_meta", "position_snapshot", "orders"} {
		var n int
		if err := db.Get(&n, `SELECT COUNT(1) FROM sqlite_master WHERE type='table' AND name=?`, tbl); err != nil {
			t.Fatalf("check %s: %v", tbl, err)
		}
		if n != 0 {
			t.Fatalf("旧表 %s 不应存在", tbl)
		}
	}
}
