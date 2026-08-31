package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestActionLog_Roundtrip(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	repo := NewActionRepo(db)
	if err := repo.Insert(ActionLog{
		Kind: "task", Name: "screener", Status: "success",
		Summary: "筛选出 3 只", DurationMs: 120,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := repo.Insert(ActionLog{
		Kind: "api", Name: "confirm_trade", Status: "success",
		Summary: "买入 000001.SZ 100股",
	}); err != nil {
		t.Fatalf("insert2: %v", err)
	}

	today := time.Now().Format("20060102")
	list, err := repo.ListByDate(today)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 logs, got %d", len(list))
	}
	if list[0].Name != "screener" || list[0].Kind != "task" {
		t.Fatalf("unexpected first log: %+v", list[0])
	}
	if list[1].Name != "confirm_trade" {
		t.Fatalf("unexpected second log: %+v", list[1])
	}
}
