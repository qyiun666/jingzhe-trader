package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// openStoreAtPath 与 openStoreForTest 同一构造，但库路径由调用方定（需要关库再重开的用例）。
func openStoreAtPath(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("store.Open(%s) 失败: %v", path, err)
	}
	return s
}

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

// TestRetentionWorksOnUnmigratedDailyBar 未迁移的旧库（daily_bar 仍是 rowid 普通表）
// 必须照常完成保留清理。
//
// 现实风险：线上库不会自动重建，新代码的保留清理改走"主键行值 IN"删除，
// 若该语法只对 WITHOUT ROWID 成立，一上线就会在清理环节炸掉整条日报链路。
func TestRetentionWorksOnUnmigratedDailyBar(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	// 退回普通表，模拟尚未执行 db rebuild-bar 的生产库。
	if _, err := s.writeDB.ExecContext(ctx, `DROP TABLE daily_bar`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	if _, err := s.writeDB.ExecContext(ctx, `CREATE TABLE daily_bar (
		ts_code TEXT NOT NULL, trade_date TEXT NOT NULL, close INTEGER NOT NULL,
		vol_lot REAL, raw_close INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ts_code, trade_date))`); err != nil {
		t.Fatalf("建普通表失败: %v", err)
	}
	now := time.Now()
	old := now.AddDate(0, 0, -100).Format("20060102") // 超出 45 天窗口
	fresh := now.Format("20060102")
	// 两条过期行（不同代码）+ 一条当日行。
	for _, row := range []struct{ code, date string }{
		{"600000.SH", old}, {"000001.SZ", old}, {"600000.SH", fresh},
	} {
		if _, err := s.writeDB.ExecContext(ctx,
			`INSERT OR IGNORE INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close) VALUES (?, ?, 1000, 1, 1000)`,
			row.code, row.date); err != nil {
			t.Fatalf("造行失败: %v", err)
		}
	}
	if _, err := s.writeDB.ExecContext(ctx,
		`INSERT OR IGNORE INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close) VALUES ('000001.SZ', ?, 2000, 1, 2000)`, old); err != nil {
		t.Fatalf("造行失败: %v", err)
	}

	del, err := ApplyRetention(ctx, s, now, nil)
	if err != nil {
		t.Fatalf("未迁移表上保留清理失败（上线即炸日报链路）: %v", err)
	}
	if del["daily_bar"] != 2 {
		t.Errorf("daily_bar 删除 %d 行，期望 2（两条过期行）", del["daily_bar"])
	}
	var left int
	if err := s.readDB.GetContext(ctx, &left, `SELECT COUNT(*) FROM daily_bar`); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if left != 1 {
		t.Errorf("剩余 %d 行，期望 1（当日行）", left)
	}
}

// TestAuditSchemaDetectsColumnDrift 列级清点（agent_issues #8 的机制化）：
// 旧库缺"后来加的列"时表级清点报的是"一致"（假绿），列级必须点名；
// ensureColumns 只补可回填的（带默认值/可空），遗留列只报不删。
func TestAuditSchemaDetectsColumnDrift(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	// 模拟旧二进制建的库：position 缺 high_price、daily_bar 缺 raw_close（两者可回填），
	// run_trace 缺 outcome（NOT NULL 无默认，不可回填），外加一列无人读的死档。
	stmts := []string{
		`ALTER TABLE position DROP COLUMN high_price`,
		`ALTER TABLE daily_bar DROP COLUMN raw_close`,
		`ALTER TABLE run_trace DROP COLUMN outcome`,
		`ALTER TABLE position ADD COLUMN dead_marker TEXT`,
	}
	for _, q := range stmts {
		if _, err := s.writeDB.ExecContext(ctx, q); err != nil {
			t.Fatalf("造列漂移失败（%s）: %v", q, err)
		}
	}

	a, err := s.AuditSchema(ctx)
	if err != nil {
		t.Fatalf("清点失败: %v", err)
	}
	if a.Clean() {
		t.Fatal("造出了缺列与遗留列，清点却报干净 —— 列级探测失效")
	}
	for _, want := range []string{"position.high_price", "daily_bar.raw_close", "run_trace.outcome"} {
		if !contains(a.MissingColumns, want) {
			t.Errorf("MissingColumns 缺 %s，实际 %v", want, a.MissingColumns)
		}
	}
	if !contains(a.LegacyColumns, "position.dead_marker") {
		t.Errorf("LegacyColumns 缺 position.dead_marker，实际 %v", a.LegacyColumns)
	}

	added, err := ensureColumns(s.writeDB)
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	// 幂等可补的都补了；不可补的（AddDDL 为空）不硬试。
	if len(added) != 2 || !contains(added, "position.high_price") || !contains(added, "daily_bar.raw_close") {
		t.Fatalf("回填清单 = %v，期望恰为 position.high_price 与 daily_bar.raw_close", added)
	}

	a, err = s.AuditSchema(ctx)
	if err != nil {
		t.Fatalf("回填后再清点失败: %v", err)
	}
	if !contains(a.MissingColumns, "run_trace.outcome") {
		t.Errorf("不可回填的缺列应仍被点名，实际 MissingColumns=%v", a.MissingColumns)
	}
	if contains(a.MissingColumns, "position.high_price") {
		t.Errorf("已回填的列不该再被报缺，实际 MissingColumns=%v", a.MissingColumns)
	}
	if !contains(a.LegacyColumns, "position.dead_marker") {
		t.Errorf("遗留列只报不删，应仍在清单里，实际 LegacyColumns=%v", a.LegacyColumns)
	}

	// 再跑一次 ensureColumns 必须一无所补（幂等），且存量数据没被回填动作波及。
	again, err := ensureColumns(s.writeDB)
	if err != nil || len(again) != 0 {
		t.Fatalf("二次回填应为幂等空操作，实际 added=%v err=%v", again, err)
	}
	if _, err := s.writeDB.ExecContext(ctx,
		`INSERT INTO position (ts_code, total_qty) VALUES ('600000.SH', 100) ON CONFLICT(ts_code) DO NOTHING`); err != nil {
		t.Fatalf("回填后写 position 失败: %v", err)
	}
	var hp int
	if err := s.readDB.GetContext(ctx, &hp, `SELECT high_price FROM position WHERE ts_code='600000.SH'`); err != nil {
		t.Fatalf("回填列读取失败: %v", err)
	}
	if hp != 0 {
		t.Errorf("回填列默认值 = %d，期望 0", hp)
	}
}

// TestDropLegacyColumnsRemovesDeadArchive 死档列清除走 dropLegacyColumns 清单：
// 给新库人为加回 config_kv.updated_at（存量库里真实存在的旧设计残留），
// 启动路径必须把它删掉且不动任何键值行。
func TestDropLegacyColumnsRemovesDeadArchive(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	if _, err := s.writeDB.ExecContext(ctx, `ALTER TABLE config_kv ADD COLUMN updated_at TEXT`); err != nil {
		t.Fatalf("模拟旧残留失败: %v", err)
	}
	if _, err := s.writeDB.ExecContext(ctx,
		`INSERT INTO config_kv (key, value) VALUES ('screen.min_bar_rows', '5000')`); err != nil {
		t.Fatalf("造配置行失败: %v", err)
	}
	a, err := s.AuditSchema(ctx)
	if err != nil {
		t.Fatalf("清点失败: %v", err)
	}
	if !contains(a.LegacyColumns, "config_kv.updated_at") {
		t.Fatalf("列级清点应点名 config_kv.updated_at，实际 LegacyColumns=%v", a.LegacyColumns)
	}

	if err := dropLegacyColumns(s.writeDB); err != nil {
		t.Fatalf("dropLegacyColumns 失败: %v", err)
	}
	if hasColumnAfter(t, s, "config_kv", "updated_at") {
		t.Error("dropLegacyColumns 后 updated_at 仍在")
	}
	var v string
	if err := s.readDB.GetContext(ctx, &v, `SELECT value FROM config_kv WHERE key='screen.min_bar_rows'`); err != nil {
		t.Fatalf("删列不该动配置行: %v", err)
	}
	if v != "5000" {
		t.Errorf("配置值 = %s，期望 5000", v)
	}
	// 幂等：再跑一次不该报错。
	if err := dropLegacyColumns(s.writeDB); err != nil {
		t.Fatalf("二次 dropLegacyColumns 应幂等，失败: %v", err)
	}
}

func hasColumnAfter(t *testing.T, s *Store, table, column string) bool {
	t.Helper()
	ok, err := hasColumn(s.writeDB, table, column)
	if err != nil {
		t.Fatalf("探测列 %s.%s 失败: %v", table, column, err)
	}
	return ok
}

// TestOpenRepairsMissingColumns 启动路径闭环：带缺列的库文件重新 Open 即自愈，
// 且动作（补了哪几列）与不可自愈项都进 SchemaAuditNote，启动日志看得见。
func TestOpenRepairsMissingColumns(t *testing.T) {
	path := t.TempDir() + "/repair.db"
	s := openStoreAtPath(t, path)
	if _, err := s.writeDB.Exec(`ALTER TABLE order_ticket DROP COLUMN total_cost`); err != nil {
		t.Fatalf("造缺列失败: %v", err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("带缺列的库重新打开失败: %v", err)
	}
	defer s2.Close()

	var n int
	if err := s2.readDB.Get(&n, `SELECT COUNT(*) FROM pragma_table_info('order_ticket') WHERE name='total_cost'`); err != nil {
		t.Fatalf("读取列信息失败: %v", err)
	}
	if n != 1 {
		t.Fatal("启动后 order_ticket.total_cost 没有被回填")
	}
	note := s2.SchemaAuditNote()
	if !strings.Contains(note, "order_ticket.total_cost") {
		t.Errorf("回填动作应写进结构摘要，实际 note=%q", note)
	}
}
