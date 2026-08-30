package store

import (
	"path/filepath"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestGetRecentAccountSnapshotsRunIDIsolation 回归: 实盘收益曲线只取 live run_id,
// 回测 bt_* 快照必须被排除 (历史上曾因裸 SELECT 漏 run_id 把回测混进实盘)
func TestGetRecentAccountSnapshotsRunIDIsolation(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	repo := NewTradeRepo(db)
	seed := []struct {
		runID, date string
		asset       float64
	}{
		{RunIDLive, "20260826", 100000},
		{RunIDLive, "20260827", 101000},
		{RunIDLive, "20260828", 102500},
		{"bt_macd", "20260827", 999999}, // 回测快照, 绝不能进实盘曲线
		{"bt_ma", "20260828", 888888},
	}
	for _, s := range seed {
		if err := repo.InsertAccountSnapshot(s.runID, model.AccountSnapshot{
			TradeDate: s.date, TotalAsset: s.asset,
		}); err != nil {
			t.Fatalf("插入快照失败: %v", err)
		}
	}

	got, err := repo.GetRecentAccountSnapshots(RunIDLive, 10)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应只返回 3 条实盘快照, 实际 %d 条: %+v", len(got), got)
	}
	for _, s := range got {
		if s.TotalAsset > 200000 {
			t.Fatalf("回测快照混入实盘: %+v", s)
		}
	}
	// 升序断言
	if got[0].TradeDate != "20260826" || got[2].TradeDate != "20260828" {
		t.Fatalf("应按日期升序, 实际首=%s 尾=%s", got[0].TradeDate, got[2].TradeDate)
	}

	// limit 生效: 取最近 2 条仍是升序
	got2, err := repo.GetRecentAccountSnapshots(RunIDLive, 2)
	if err != nil {
		t.Fatalf("limit 查询失败: %v", err)
	}
	if len(got2) != 2 || got2[0].TradeDate != "20260827" || got2[1].TradeDate != "20260828" {
		t.Fatalf("limit=2 应返回最近两日升序, 实际 %+v", got2)
	}
}
