package store

import (
	"fmt"
	"testing"
)

// TestDeleteRowsBatched 分批删除应删除全部满足条件行, 且跨批 (数据量 > batchSize) 时结果正确
func TestDeleteRowsBatched(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	// 插入 25 行不同交易日: 20 行旧数据 (2025年), 5 行新数据 (2026年)
	for i := 0; i < 25; i++ {
		date := fmt.Sprintf("2025%04d", 101+i) // 2025 成交日递增
		if i >= 20 {
			date = fmt.Sprintf("2026%04d", 101+i-20)
		}
		if _, err := db.Exec(`INSERT INTO daily_bar (ts_code, trade_date, open, high, low, close, pre_close, change, pct_chg, vol, amount, adj_factor)
			VALUES ('600519.SH', ?, 1, 1, 1, 1, 1, 0, 0, 0, 0, 1)`, date); err != nil {
			t.Fatalf("插入测试数据失败: %v", err)
		}
	}

	// batchSize=7 强制跨批 (20 行需 3 批)
	if err := deleteRowsBatched(db, "daily_bar", `trade_date < ?`, 7, "20260101"); err != nil {
		t.Fatalf("deleteRowsBatched: %v", err)
	}

	var remain int
	if err := db.Get(&remain, `SELECT COUNT(1) FROM daily_bar`); err != nil {
		t.Fatalf("查询剩余行数失败: %v", err)
	}
	if remain != 5 {
		t.Fatalf("应剩 5 行, 实际 %d", remain)
	}
}

// TestDeleteRowsBatchedEmpty 无匹配行时应正常返回且不报错
func TestDeleteRowsBatchedEmpty(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()

	if err := deleteRowsBatched(db, "daily_bar", `trade_date < ?`, 0, "20200101"); err != nil {
		t.Fatalf("空删除不应报错: %v", err)
	}
}
