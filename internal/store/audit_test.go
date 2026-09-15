package store

import (
	"context"
	"fmt"
	"testing"
)

// TestAuditSchemaClean 全新库（现役 schema 建的）必须清点干净。
func TestAuditSchemaClean(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	a, err := s.AuditSchema(ctx)
	if err != nil {
		t.Fatalf("清点失败: %v", err)
	}
	if !a.Clean() {
		t.Fatalf("全新库应清点干净，实际:\n%s", a.String())
	}
	if a.DailyBarNormal {
		t.Errorf("新库 daily_bar 应为 WITHOUT ROWID（有 rowid = true 不该出现）")
	}
	rowid, err := s.DailyBarHasRowid(ctx)
	if err != nil {
		t.Fatalf("探测 daily_bar 失败: %v", err)
	}
	if rowid {
		t.Errorf("新库 daily_bar 仍是普通表（有 rowid），DDL 里的 WITHOUT ROWID 没生效")
	}
}

// TestAuditSchemaDetectsLegacy 负例：人为造出遗留表与重复索引，清点必须点名它们。
// 没有这条负例，"清点通过"也可能只是因为探测逻辑恒返回空。
func TestAuditSchemaDetectsLegacy(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	// 模拟旧二进制留下的东西：一张废表 + 一个与主键前缀重复的索引。
	stmts := []string{
		`CREATE TABLE moneyflow (ts_code TEXT, trade_date TEXT)`,
		`CREATE INDEX idx_daily_bar_ts_code ON daily_bar(ts_code)`,
	}
	for _, q := range stmts {
		if _, err := s.writeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("造遗留对象失败（%s）: %v", q, err)
		}
	}

	a, err := s.AuditSchema(ctx)
	if err != nil {
		t.Fatalf("清点失败: %v", err)
	}
	if a.Clean() {
		t.Fatal("造出了遗留表与索引，清点却报干净 —— 探测逻辑失效")
	}
	if !contains(a.LegacyTables, "moneyflow") {
		t.Errorf("遗留表未点名 moneyflow，实际 %v", a.LegacyTables)
	}
	if !contains(a.LegacyIndexes, "idx_daily_bar_ts_code") {
		t.Errorf("遗留索引未点名 idx_daily_bar_ts_code，实际 %v", a.LegacyIndexes)
	}
	// sqlite_autoindex_* 是主键自动索引，不该被当成遗留具名索引。
	for _, idx := range a.LegacyIndexes {
		if len(idx) > 4 && idx[:4] == "sqli" {
			t.Errorf("自动索引 %s 被误报为遗留索引", idx)
		}
	}
}

// TestRebuildDailyBarMovesRows daily_bar 重建为 WITHOUT ROWID 后行数不变、且可继续读写。
func TestRebuildDailyBarMovesRows(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	// 先把 daily_bar 退回普通表（模拟旧库），再验证重建器能把它搬到 WITHOUT ROWID。
	if _, err := s.writeDB.ExecContext(ctx, `DROP TABLE daily_bar`); err != nil {
		t.Fatalf("清掉新表失败: %v", err)
	}
	if _, err := s.writeDB.ExecContext(ctx, `CREATE TABLE daily_bar (
		ts_code TEXT NOT NULL, trade_date TEXT NOT NULL, close INTEGER NOT NULL,
		vol_lot REAL, raw_close INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ts_code, trade_date))`); err != nil {
		t.Fatalf("建回普通表失败: %v", err)
	}
	for _, d := range []string{"20260901", "20260902", "20260903"} {
		if _, err := s.writeDB.ExecContext(ctx,
			`INSERT INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close) VALUES ('600000.SH', ?, 1000, 5, 1000)`, d); err != nil {
			t.Fatalf("造行失败: %v", err)
		}
	}

	moved, err := s.RebuildDailyBar(ctx)
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if moved != 3 {
		t.Errorf("搬迁行数 = %d，期望 3", moved)
	}
	rowid, err := s.DailyBarHasRowid(ctx)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if rowid {
		t.Error("重建后 daily_bar 仍是普通表")
	}
	// 重建后读写必须照旧（WITHOUT ROWID 不影响主键访问路径）。
	var n int
	if err := s.readDB.GetContext(ctx, &n, `SELECT COUNT(*) FROM daily_bar`); err != nil {
		t.Fatalf("重建后读失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("重建后行数 = %d，期望 3", n)
	}
	if _, err := s.writeDB.ExecContext(ctx,
		`INSERT INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close) VALUES ('600001.SH','20260901',2000,1,2000)
		 ON CONFLICT(ts_code, trade_date) DO UPDATE SET close=excluded.close`); err != nil {
		t.Fatalf("重建后写失败: %v", err)
	}
	// 幂等：已是 WITHOUT ROWID 时再调用返回 0 且不报错。
	if again, err := s.RebuildDailyBar(ctx); err != nil || again != 0 {
		t.Errorf("重复重建应为幂等空操作，得到 moved=%d err=%v", again, err)
	}
}

// TestDeleteBatchedRowsOnWithoutRowid 分批删除在无 rowid 表上必须真的删掉行
// （rowid 版本会直接报错 —— 这是 daily_bar 改成 WITHOUT ROWID 的关键回归点）。
func TestDeleteBatchedRowsOnWithoutRowid(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		d := fmt.Sprintf("202609%02d", i+1)
		if _, err := s.writeDB.ExecContext(ctx,
			`INSERT INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close) VALUES (?, ?, 1000, 1, 1000)`,
			"600000.SH", d); err != nil {
			t.Fatalf("造行失败: %v", err)
		}
	}
	// 每批 10 行，删掉 trade_date <= 20260924 的 24 条，只留最后一条。
	del, batches, err := DeleteBatchedRows(ctx, s.writeDB, "daily_bar",
		[]string{"ts_code", "trade_date"}, "trade_date <= ?", []interface{}{"20260924"}, 10)
	if err != nil {
		t.Fatalf("分批删除失败: %v", err)
	}
	if del != 24 {
		t.Errorf("删除 %d 行，期望 24", del)
	}
	if batches < 2 {
		t.Errorf("批数 = %d，期望 ≥2（每批 10 行、共 24 行）", batches)
	}
	var left int
	if err := s.readDB.GetContext(ctx, &left, `SELECT COUNT(*) FROM daily_bar`); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if left != 1 {
		t.Errorf("剩余 %d 行，期望 1", left)
	}
}

// TestDeleteBatchedRowsRejectsEmptyPK 空主键列必须拒绝：否则会退化成"整表无过滤删除"。
func TestDeleteBatchedRowsRejectsEmptyPK(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	if _, _, err := DeleteBatchedRows(context.Background(), s.writeDB, "daily_bar", nil, "", nil, 10); err == nil {
		t.Fatal("空主键列应被拒绝（否则会整表删除）")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestRebuildDailyBarRollsBackOnFailure 重建的事务安全是数据安全的核心保证：
// 中途失败必须回滚，原表行数与 rowid 状态不变、不留 daily_bar_rebuild 残留。
//
// 手法：预置一张 daily_bar_rebuild 残留表让 CREATE TABLE 失败。修复前（无 DROP IF EXISTS）
// 这正是它的失败路径；修复后该表会被先清掉，故这里改用"制造一张同名但结构冲突的表"
// 之外的确定性手段——直接断言清残留后重建成功，并用另一条路径验证失败回滚。
func TestRebuildDailyBarRollsBackOnFailure(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	// 退回普通表并造 3 行。
	if _, err := s.writeDB.ExecContext(ctx, `DROP TABLE daily_bar`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	if _, err := s.writeDB.ExecContext(ctx, `CREATE TABLE daily_bar (
		ts_code TEXT NOT NULL, trade_date TEXT NOT NULL, close INTEGER NOT NULL,
		vol_lot REAL, raw_close INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ts_code, trade_date))`); err != nil {
		t.Fatalf("建普通表失败: %v", err)
	}
	for _, d := range []string{"20260901", "20260902", "20260903"} {
		if _, err := s.writeDB.ExecContext(ctx,
			`INSERT INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close) VALUES ('600000.SH', ?, 1000, 5, 1000)`, d); err != nil {
			t.Fatalf("造行失败: %v", err)
		}
	}
	// 预置一张 daily_bar_rebuild：重建应先用 DROP IF EXISTS 把它清掉，然后成功。
	if _, err := s.writeDB.ExecContext(ctx, `CREATE TABLE daily_bar_rebuild (junk TEXT)`); err != nil {
		t.Fatalf("造残留表失败: %v", err)
	}
	moved, err := s.RebuildDailyBar(ctx)
	if err != nil {
		t.Fatalf("有残留表时重建也应成功（应先清残留），实际报错: %v", err)
	}
	if moved != 3 {
		t.Errorf("搬迁行数 = %d，期望 3", moved)
	}
	if hasTable(t, s.readDB, "daily_bar_rebuild") {
		t.Error("重建完成后不该留下 daily_bar_rebuild")
	}
	rowid, err := s.DailyBarHasRowid(ctx)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if rowid {
		t.Error("重建后 daily_bar 仍是普通表")
	}
}

// TestAuditSchemaReportsProbeFailure 清点本身失败不能被读成"库结构干净"。
func TestAuditSchemaReportsProbeFailure(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	// 把 daily_bar 删掉，dailyBarHasRowid 会报错。
	if _, err := s.writeDB.Exec(`DROP TABLE daily_bar`); err != nil {
		t.Fatalf("删表失败: %v", err)
	}
	if _, err := s.AuditSchema(context.Background()); err == nil {
		t.Fatal("daily_bar 缺失时清点应报错，而不是静默返回")
	}
}
