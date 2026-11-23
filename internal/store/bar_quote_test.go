package store

import (
	"context"
	"database/sql"
	"testing"

	"jingzhe-trader/internal/model"
)

// fenp 取地址助手（测试里写 BarQuote 的三态用）。
func fenp(v int64) *model.Fen {
	f := model.Fen(v)
	return &f
}

// readQuote 按主键读回一行的 5 列扩展价族；NULL 一律回 nil（区分"没数据"与"0 价"）。
func readQuote(t *testing.T, s *Store, tsCode, date string) (BarQuote, bool) {
	t.Helper()
	var o, h, l, up, dn sql.NullInt64
	err := s.readDB.QueryRowContext(context.Background(),
		`SELECT open, high, low, up_limit, down_limit FROM daily_bar
			WHERE ts_code=? AND trade_date=?`, tsCode, date).
		Scan(&o, &h, &l, &up, &dn)
	if err == sql.ErrNoRows {
		return BarQuote{}, false
	}
	if err != nil {
		t.Fatalf("读扩展价族失败: %v", err)
	}
	pick := func(v sql.NullInt64) *model.Fen {
		if !v.Valid {
			return nil
		}
		f := model.Fen(v.Int64)
		return &f
	}
	return BarQuote{Open: pick(o), High: pick(h), Low: pick(l), UpLimit: pick(up), DownLimit: pick(dn)}, true
}

func quoteVal(t *testing.T, col string, v *model.Fen) int64 {
	t.Helper()
	if v == nil {
		t.Fatalf("%s 是 NULL（unknown），期望有值", col)
	}
	return int64(*v)
}

// TestUpdateBarQuoteIsUpdateOnly 锁死两条口径：
//  1. 只更新既有行 —— 主键不存在时返回 hit=false 且**绝不造行**（stk_limit 比 daily
//     多出的代码不许在 daily_bar 里凭空长出一根没有收盘价的假 bar）；
//  2. nil 写 NULL 而不是 0 —— unknown 与"价格 0.00 元"必须在库里可分。
func TestUpdateBarQuoteIsUpdateOnly(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	rc := s.MarketRepo()

	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: "20260901",
		Close: 1050, VolLot: 5, RawClose: 1050}); err != nil {
		t.Fatalf("造既有行失败: %v", err)
	}

	hit, err := rc.UpdateBarQuote(ctx, "600000.SH", "20260901", BarQuote{
		Open: fenp(1010), High: fenp(1080), Low: fenp(1000),
		UpLimit: fenp(1155), DownLimit: nil, // 跌停价缺失 → 必须落 NULL
	})
	if err != nil {
		t.Fatalf("补齐失败: %v", err)
	}
	if !hit {
		t.Fatal("已存在的行 UPDATE 返回未命中")
	}
	q, ok := readQuote(t, s, "600000.SH", "20260901")
	if !ok {
		t.Fatal("行不见了")
	}
	if got := quoteVal(t, "open", q.Open); got != 1010 {
		t.Errorf("open=%d 期望 1010", got)
	}
	if got := quoteVal(t, "high", q.High); got != 1080 {
		t.Errorf("high=%d 期望 1080", got)
	}
	if got := quoteVal(t, "low", q.Low); got != 1000 {
		t.Errorf("low=%d 期望 1000", got)
	}
	if got := quoteVal(t, "up_limit", q.UpLimit); got != 1155 {
		t.Errorf("up_limit=%d 期望 1155", got)
	}
	if q.DownLimit != nil {
		t.Errorf("down_limit 写成了 %d，unknown 必须是 NULL 而不是 0", int64(*q.DownLimit))
	}
	// 收盘价不能被扩展价族的 UPDATE 波及（两条写路径各管各的列）。
	var closeFen int64
	if err := s.readDB.GetContext(ctx, &closeFen,
		`SELECT close FROM daily_bar WHERE ts_code='600000.SH' AND trade_date='20260901'`); err != nil {
		t.Fatalf("读 close 失败: %v", err)
	}
	if closeFen != 1050 {
		t.Errorf("close 被改写成 %d，期望 1050", closeFen)
	}

	// 未命中：库里没有这根 bar，UPDATE 不得把它"补"出来。
	before := barRowCount(t, s)
	missHit, err := rc.UpdateBarQuote(ctx, "900999.SH", "20260901", BarQuote{
		Open: fenp(1), High: fenp(2), Low: fenp(3), UpLimit: fenp(4), DownLimit: fenp(5),
	})
	if err != nil {
		t.Fatalf("未命中路径不该报错: %v", err)
	}
	if missHit {
		t.Error("不存在的行返回命中，RowsAffected 判据失效")
	}
	if after := barRowCount(t, s); after != before {
		t.Fatalf("UPDATE-only 造出了行：前 %d 后 %d（假 bar 会污染覆盖缺口闸门）", before, after)
	}
	if _, ok := readQuote(t, s, "900999.SH", "20260901"); ok {
		t.Fatal("900999.SH 不该存在于 daily_bar")
	}
}

func barRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.readDB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM daily_bar`).Scan(&n); err != nil {
		t.Fatalf("数 daily_bar 行数失败: %v", err)
	}
	return n
}

// TestEnsureColumnsAddsNullableQuoteColumns 存量库（无这 5 列）启动期必须幂等补出，
// 且存量行取 NULL（unknown）而不是 0 —— 这是"加列不给旧行编造价格"的机制保证。
func TestEnsureColumnsAddsNullableQuoteColumns(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	for _, col := range []string{"open", "high", "low", "up_limit", "down_limit"} {
		if _, err := s.writeDB.ExecContext(ctx, `ALTER TABLE daily_bar DROP COLUMN `+col); err != nil {
			t.Fatalf("模拟旧库（删掉 %s）失败: %v", col, err)
		}
	}
	if err := s.MarketRepo().UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: "20260901",
		Close: 1000, VolLot: 1, RawClose: 1000}); err != nil {
		t.Fatalf("造旧行失败: %v", err)
	}

	added, err := ensureColumns(s.writeDB)
	if err != nil {
		t.Fatalf("补列失败: %v", err)
	}
	// ensureColumns 的返回值形态是 "表.列"（与 AuditSchema 同一坐标写法）。
	for _, want := range []string{"daily_bar.open", "daily_bar.high", "daily_bar.low",
		"daily_bar.up_limit", "daily_bar.down_limit"} {
		if !contains(added, want) {
			t.Errorf("补列清单没点到 %s，实际 %v", want, added)
		}
	}
	if len(added) != 5 {
		t.Errorf("补列 %d 项，期望恰为这 5 列：%v", len(added), added)
	}
	// 幂等：再跑一次一无所补。
	again, err := ensureColumns(s.writeDB)
	if err != nil || len(again) != 0 {
		t.Fatalf("二次补列应幂等，实际 added=%v err=%v", again, err)
	}
	// 旧行的 5 列必须是 NULL。
	q, ok := readQuote(t, s, "600000.SH", "20260901")
	if !ok {
		t.Fatal("补列过程把旧行弄丢了")
	}
	for name, v := range map[string]*model.Fen{"open": q.Open, "high": q.High, "low": q.Low,
		"up_limit": q.UpLimit, "down_limit": q.DownLimit} {
		if v != nil {
			t.Errorf("补列后 %s=%d，旧行应为 NULL（unknown）而不是 0", name, int64(*v))
		}
	}
	// 补列后库结构清点必须报干净。
	a, err := s.AuditSchema(ctx)
	if err != nil {
		t.Fatalf("清点失败: %v", err)
	}
	if len(a.MissingColumns) != 0 || len(a.LegacyColumns) != 0 {
		t.Errorf("补列后仍有列差异 missing=%v legacy=%v", a.MissingColumns, a.LegacyColumns)
	}
}

// TestRebuildDailyBarKeepsExtendedQuotes 离线重建必须带上 5 列扩展价族。
//
// 这条断言防的是最坏的一种失败：重建器按硬编码的旧 5 列搬迁，新表根本没有这 5 列，
// DROP 之后已填的涨跌停价整批消失；而下次启动 ensureColumns 会把缺列补成 NULL，
// `db audit` 照样报"库结构与现役 schema 一致" —— 数据没了，监控看着没病。
func TestRebuildDailyBarKeepsExtendedQuotes(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	if _, err := s.writeDB.ExecContext(ctx, `DROP TABLE daily_bar`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	// 旧库形状一：普通表（有 rowid）+ 全部 10 列，且扩展价族已填过值。
	if _, err := s.writeDB.ExecContext(ctx, `CREATE TABLE daily_bar (
		ts_code TEXT NOT NULL, trade_date TEXT NOT NULL, close INTEGER NOT NULL,
		vol_lot REAL, raw_close INTEGER NOT NULL DEFAULT 0,
		open INTEGER, high INTEGER, low INTEGER, up_limit INTEGER, down_limit INTEGER,
		PRIMARY KEY (ts_code, trade_date))`); err != nil {
		t.Fatalf("建旧库失败: %v", err)
	}
	if _, err := s.writeDB.ExecContext(ctx, `INSERT INTO daily_bar VALUES
		('600000.SH','20260901',1050,5,1050,1010,1080,1000,1155,945),
		('600001.SH','20260901',2000,1,2000,NULL,NULL,NULL,NULL,NULL)`); err != nil {
		t.Fatalf("造旧行失败: %v", err)
	}

	moved, err := s.RebuildDailyBar(ctx)
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if moved != 2 {
		t.Errorf("搬迁行数 = %d，期望 2", moved)
	}
	// 列集合必须与现役清单一字不差（双向）。
	got, err := s.tableColumns(ctx, "daily_bar")
	if err != nil {
		t.Fatalf("读列失败: %v", err)
	}
	var want []string
	for _, tc := range SchemaColumns {
		if tc.Table == "daily_bar" {
			for _, c := range tc.Columns {
				want = append(want, c.Name)
			}
		}
	}
	if len(got) != len(want) {
		t.Fatalf("重建后列数 %d ≠ 现役清单 %d（%v）", len(got), len(want), want)
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("重建后丢了列 %s —— 扩展价族会被抹成 NULL 且清点看不出来", name)
		}
	}

	q, ok := readQuote(t, s, "600000.SH", "20260901")
	if !ok {
		t.Fatal("重建后 600000.SH 的行不见了")
	}
	for name, pair := range map[string][2]*model.Fen{
		"open": {q.Open, fenp(1010)}, "high": {q.High, fenp(1080)}, "low": {q.Low, fenp(1000)},
		"up_limit": {q.UpLimit, fenp(1155)}, "down_limit": {q.DownLimit, fenp(945)},
	} {
		if pair[0] == nil || *pair[0] != *pair[1] {
			t.Errorf("重建后 %s = %v，期望原值 %d", name, pair[0], int64(*pair[1]))
		}
	}
	// 原本 unknown 的行重建后仍是 NULL，不得被填成 0。
	q2, ok := readQuote(t, s, "600001.SH", "20260901")
	if !ok {
		t.Fatal("重建后 600001.SH 的行不见了")
	}
	if q2.Open != nil || q2.UpLimit != nil {
		t.Errorf("重建把 unknown 写成了值：open=%v up_limit=%v", q2.Open, q2.UpLimit)
	}
}

// TestRebuildDailyBarFromNarrowLegacyTable 更老的库（连扩展价族列都没有）也必须能重建：
// 搬迁列清单按库里真实有的列取，缺的在新表保持 NULL。硬列清单会让这条路径在
// "no such column: open" 上永久失败，而重建是拿到 WITHOUT ROWID 的唯一手段。
func TestRebuildDailyBarFromNarrowLegacyTable(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	if _, err := s.writeDB.ExecContext(ctx, `DROP TABLE daily_bar`); err != nil {
		t.Fatalf("清表失败: %v", err)
	}
	if _, err := s.writeDB.ExecContext(ctx, `CREATE TABLE daily_bar (
		ts_code TEXT NOT NULL, trade_date TEXT NOT NULL, close INTEGER NOT NULL,
		vol_lot REAL, raw_close INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (ts_code, trade_date))`); err != nil {
		t.Fatalf("建窄表失败: %v", err)
	}
	if _, err := s.writeDB.ExecContext(ctx,
		`INSERT INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close) VALUES ('600000.SH','20260901',1000,5,1000)`); err != nil {
		t.Fatalf("造行失败: %v", err)
	}

	moved, err := s.RebuildDailyBar(ctx)
	if err != nil {
		t.Fatalf("窄旧表重建失败（列清单没按库里真实列取？）: %v", err)
	}
	if moved != 1 {
		t.Errorf("搬迁行数 = %d，期望 1", moved)
	}
	q, ok := readQuote(t, s, "600000.SH", "20260901")
	if !ok {
		t.Fatal("重建后行不见了")
	}
	if q.Open != nil || q.High != nil || q.Low != nil || q.UpLimit != nil || q.DownLimit != nil {
		t.Errorf("窄旧表重建后扩展价族应全为 NULL，实际 %+v", q)
	}
	if rowid, err := s.DailyBarHasRowid(ctx); err != nil || rowid {
		t.Errorf("重建后仍是普通表 rowid=%v err=%v", rowid, err)
	}
}
