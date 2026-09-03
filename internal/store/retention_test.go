package store

import (
	"context"
	"testing"
	"time"
)

// openStoreForTest 打开临时库（自动建全部表），供 store 包测试使用。
func openStoreForTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/ret.db")
	if err != nil {
		t.Fatalf("store.Open 失败: %v", err)
	}
	return s
}

// TestDeleteBatched100k 对应验收 #11：10 万行、批上限 5000，应恰好 20 批删完。
func TestDeleteBatched100k(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	const n = 100_000
	cols := []string{"job_name", "trade_date", "status", "started_at"}
	rows := make([][]interface{}, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, []interface{}{
			"job_" + itoa(i),
			"20000101",
			"ok",
			"2000-01-01T00:00:00Z",
		})
	}
	if _, err := BatchInsert(ctx, s.WriteDB(), "job_run", cols, rows, 2000); err != nil {
		t.Fatalf("批量插入 100k 行失败: %v", err)
	}
	var cnt int
	if err := s.WriteDB().Get(&cnt, "SELECT COUNT(*) FROM job_run"); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if cnt != n {
		t.Fatalf("期望插入 %d 行，实际 %d", n, cnt)
	}

	deleted, batches, err := DeleteBatched(ctx, s.WriteDB(), "job_run", "trade_date < ?", []interface{}{"20990101"}, 5000)
	if err != nil {
		t.Fatalf("DeleteBatched 失败: %v", err)
	}
	if deleted != n {
		t.Fatalf("期望删除 %d 行，实际 %d", n, deleted)
	}
	if batches != n/5000 {
		t.Fatalf("期望 %d 批，实际 %d", n/5000, batches)
	}
	if err := s.WriteDB().Get(&cnt, "SELECT COUNT(*) FROM job_run"); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("删除后应清空，剩余 %d", cnt)
	}
}

// TestDeleteBatchedTimeout 对应验收 #11：context 已过期时 DeleteBatched 应立即以
// DeadlineExceeded 提前退出（保留剩余，不阻塞）。
func TestDeleteBatchedTimeout(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()

	cols := []string{"job_name", "trade_date", "status", "started_at"}
	const n = 5000
	rows := make([][]interface{}, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, []interface{}{"t_" + itoa(i), "20000101", "ok", "2000-01-01T00:00:00Z"})
	}
	if _, err := BatchInsert(context.Background(), s.WriteDB(), "job_run", cols, rows, 2000); err != nil {
		t.Fatalf("批量插入失败: %v", err)
	}

	// 已过期 context
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	// 确保已过期
	time.Sleep(2 * time.Millisecond)

	deleted, _, err := DeleteBatched(ctx, s.WriteDB(), "job_run", "trade_date < ?", []interface{}{"20990101"}, 5000)
	if err == nil {
		t.Fatal("期望返回 DeadlineExceeded，但无错误")
	}
	if err != context.DeadlineExceeded {
		t.Fatalf("期望 context.DeadlineExceeded，实际 %v", err)
	}
	if deleted != 0 {
		t.Fatalf("已过期 context 应提前退出且删除 0 行，实际 %d", deleted)
	}
}

// TestApplyRetentionSmoke ApplyRetention 在空库上遍历全部策略表不应 panic/报错。
func TestApplyRetentionSmoke(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()

	now := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC)
	results, err := ApplyRetention(context.Background(), s, now, nil)
	if err != nil {
		t.Fatalf("ApplyRetention 失败: %v", err)
	}
	// 空库各表删除数应为 0
	for tbl, del := range results {
		if del != 0 {
			t.Fatalf("空库 %s 不应有删除，实际 %d", tbl, del)
		}
	}
}

// itoa 简单整型转字符串（测试内用，避免额外依赖）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
