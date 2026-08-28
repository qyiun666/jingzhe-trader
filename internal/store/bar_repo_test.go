package store

import (
	"path/filepath"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestBarRepoGetLatestBars 每只代码各取其最新一根日线 (停牌股取停牌前收盘)
func TestBarRepoGetLatestBars(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	repo := NewBarRepo(db)
	bars := []model.Bar{
		{TsCode: "600519.SH", TradeDate: "20260826", Close: 1480},
		{TsCode: "600519.SH", TradeDate: "20260828", Close: 1500},
		{TsCode: "000001.SZ", TradeDate: "20260822", Close: 10.5}, // 之后停牌, 无更新行情
		{TsCode: "300750.SZ", TradeDate: "20260828", Close: 200},  // 不在查询范围
	}
	if err := repo.BatchInsert(bars); err != nil {
		t.Fatalf("插入日线失败: %v", err)
	}

	got, err := repo.GetLatestBars([]string{"600519.SH", "000001.SZ"})
	if err != nil {
		t.Fatalf("查询最新日线失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应返回 2 条, 实际 %d 条: %+v", len(got), got)
	}
	byCode := make(map[string]model.Bar, len(got))
	for _, b := range got {
		byCode[b.TsCode] = b
	}
	if b := byCode["600519.SH"]; b.TradeDate != "20260828" || b.Close != 1500 {
		t.Errorf("600519.SH 应取 20260828 收盘 1500, 实际 %s/%v", b.TradeDate, b.Close)
	}
	if b := byCode["000001.SZ"]; b.TradeDate != "20260822" || b.Close != 10.5 {
		t.Errorf("000001.SZ 应取停牌前 20260822 收盘 10.5, 实际 %s/%v", b.TradeDate, b.Close)
	}
}
