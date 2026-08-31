package store

import (
	"path/filepath"
	"testing"
)

func newScreenRepo(t *testing.T) *ScreenRepo {
	t.Helper()
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewScreenRepo(db)
}

// TestGetLatestDateEmptyTable 空表必须返回空日期而非报错: 新库尚未选股时, 调度器的
// 新鲜度检查与策略池合并都走这条路径, 报错会让当天直接不出计划
func TestGetLatestDateEmptyTable(t *testing.T) {
	r := newScreenRepo(t)
	got, err := r.GetLatestDate()
	if err != nil {
		t.Fatalf("空表不应报错: %v", err)
	}
	if got != "" {
		t.Fatalf("空表应返回空日期, 实际 %q", got)
	}
	codes, err := r.GetScreenedCodes("20260831")
	if err != nil || len(codes) != 0 {
		t.Fatalf("空表应返回空候选列表, 实际 codes=%v err=%v", codes, err)
	}
}

// TestGetScreenedCodesByDate 只返回本次运行交易日的候选, 过期日期返回空
func TestGetScreenedCodesByDate(t *testing.T) {
	r := newScreenRepo(t)
	save := func(date string, results []ScreenResult) {
		if err := r.SaveResults(date, results); err != nil {
			t.Fatalf("保存选股结果失败: %v", err)
		}
	}
	save("20260831", []ScreenResult{{TsCode: "600001.SH", TradeDate: "20260831", Score: 1}, {TsCode: "600002.SH", TradeDate: "20260831", Score: 9}})

	got, err := r.GetScreenedCodes("20260831")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 2 || got[0] != "600002.SH" {
		t.Fatalf("应按评分降序返回当日候选, 实际 %v", got)
	}
	// 日期错一位 (选股任务当天没跑成) 就不能把旧候选当今日股票池
	if stale, _ := r.GetScreenedCodes("20260901"); len(stale) != 0 {
		t.Fatalf("过期选股结果不应被合并, 实际 %v", stale)
	}
}
