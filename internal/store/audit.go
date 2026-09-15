package store

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
)

// SchemaAudit 库结构与现役 schema 的差异清点结果。
//
// 为什么需要它：建表 DDL 全是 `IF NOT EXISTS`，只做加法。一次跨版本重写之后，
// 旧二进制留下的表与索引会永远躺在库里 —— 既占空间（2026-09-15 实测：一个开发库
// 31MB 里 16 张 0 行废表 + 2 个与主键重复的索引占了 16MB），又让"这张表还有人读吗"
// 无法回答。这里只清点、不自动删：删表是不可逆动作，交给人拿这份清单决定。
type SchemaAudit struct {
	LegacyTables  []string // 库里有、现役 schema 里没有的表
	MissingTables []string // 现役 schema 里该有、库里没有的表
	LegacyIndexes []string // 库里有、现役 schema 里没有的具名索引
	// DailyBarNormal bool= daily_bar 仍是普通表（有 rowid）。false 表示已按现役
	// schema 建成 WITHOUT ROWID。旧库需要 offline 重建才能拿到省下的主键索引空间。
	DailyBarNormal bool
}

// Clean 是否与现役 schema 完全一致（遗留物为空且 daily_bar 已是 WITHOUT ROWID）。
func (a SchemaAudit) Clean() bool {
	return len(a.LegacyTables) == 0 && len(a.MissingTables) == 0 &&
		len(a.LegacyIndexes) == 0 && !a.DailyBarNormal
}

// String 人读摘要（启动日志与 CLI 输出共用，避免两处各写一套文案）。
func (a SchemaAudit) String() string {
	if a.Clean() {
		return "库结构与现役 schema 一致"
	}
	var b strings.Builder
	if len(a.MissingTables) > 0 {
		fmt.Fprintf(&b, "缺失表 %d（重跑建表可补）: %s\n", len(a.MissingTables), strings.Join(a.MissingTables, ", "))
	}
	if len(a.LegacyTables) > 0 {
		fmt.Fprintf(&b, "遗留表 %d（现役代码无读者，确认后 DROP）: %s\n", len(a.LegacyTables), strings.Join(a.LegacyTables, ", "))
	}
	if len(a.LegacyIndexes) > 0 {
		fmt.Fprintf(&b, "遗留索引 %d（现役代码未建，多数与主键重复）: %s\n", len(a.LegacyIndexes), strings.Join(a.LegacyIndexes, ", "))
	}
	if a.DailyBarNormal {
		b.WriteString("daily_bar 仍是普通表（有 rowid）：改 WITHOUT ROWID 可省下一棵主键索引，需 offline 重建（jingzhe db rebuild-bar）\n")
	}
	return b.String()
}

// AuditSchema 清点库结构与现役 schema 的差异。只读。
func (s *Store) AuditSchema(ctx context.Context) (SchemaAudit, error) {
	var a SchemaAudit
	have, err := s.objectNames(ctx, "table")
	if err != nil {
		return a, err
	}
	want := make(map[string]bool, len(SchemaTables))
	for _, t := range SchemaTables {
		want[t] = true
		if !have[t] {
			a.MissingTables = append(a.MissingTables, t)
		}
	}
	for name := range have {
		if !want[name] {
			a.LegacyTables = append(a.LegacyTables, name)
		}
	}

	idx, err := s.objectNames(ctx, "index")
	if err != nil {
		return a, err
	}
	wantIdx := make(map[string]bool, len(SchemaIndexes))
	for _, i := range SchemaIndexes {
		wantIdx[i] = true
	}
	for name := range idx {
		// sqlite_autoindex_* 是主键/唯一约束自动生成的，不属于命名索引。
		if !wantIdx[name] && !strings.HasPrefix(name, "sqlite_autoindex_") {
			a.LegacyIndexes = append(a.LegacyIndexes, name)
		}
	}
	sort.Strings(a.LegacyTables)
	sort.Strings(a.MissingTables)
	sort.Strings(a.LegacyIndexes)

	rowid, err := s.dailyBarHasRowid(ctx)
	if err != nil {
		return a, err
	}
	a.DailyBarNormal = rowid
	return a, nil
}

// objectNames 返回库内指定类型（table/index）的全部对象名集合。
func (s *Store) objectNames(ctx context.Context, typ string) (map[string]bool, error) {
	var names []string
	q := `SELECT name FROM sqlite_master WHERE type = ? AND name NOT LIKE 'sqlite_%'`
	if err := s.readDB.SelectContext(ctx, &names, q, typ); err != nil {
		return nil, fmt.Errorf("读取库内 %s 清单失败: %w", typ, err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// DailyBarHasRowid 报告 daily_bar 是否是普通表（有 rowid）。
// 判定依据是建表 SQL 文本里的 WITHOUT ROWID：表已存在时 PRAGMA 无法直接回答这一点。
func (s *Store) DailyBarHasRowid(ctx context.Context) (bool, error) { return s.dailyBarHasRowid(ctx) }

func (s *Store) dailyBarHasRowid(ctx context.Context) (bool, error) {
	var ddl string
	err := s.readDB.GetContext(ctx, &ddl,
		`SELECT COALESCE(sql,'') FROM sqlite_master WHERE type='table' AND name='daily_bar'`)
	if err != nil {
		return false, fmt.Errorf("读取 daily_bar 建表语句失败: %w", err)
	}
	if ddl == "" {
		return false, fmt.Errorf("库里没有 daily_bar 表")
	}
	return !strings.Contains(strings.ToUpper(ddl), "WITHOUT ROWID"), nil
}

// RebuildDailyBar 把 daily_bar 重建为 WITHOUT ROWID，返回搬迁的行数。
//
// 只在离线执行（没有其他进程在写库）——重建期间要 DROP 原表。全过程在一个事务里：
// 复制行数核对通过才替换，核对不过直接回滚，原表不动；索引在同事务内重建，
// 不留"表在索引没了"的中间态。
func (s *Store) RebuildDailyBar(ctx context.Context) (int, error) {
	rowid, err := s.dailyBarHasRowid(ctx)
	if err != nil {
		return 0, err
	}
	if !rowid {
		return 0, nil // 已是 WITHOUT ROWID，幂等返回
	}

	var moved int
	err = WithTx(ctx, s.writeDB, func(tx *sqlx.Tx) error {
		for _, q := range []string{
			// 先清残留：上一次重建若在 DROP 之后异常中断，daily_bar_rebuild 会留在库里，
			// 而 CREATE TABLE 没有 IF NOT EXISTS，会让本次重建永久失败且不自愈。
			`DROP TABLE IF EXISTS daily_bar_rebuild`,
			`CREATE TABLE daily_bar_rebuild (
				ts_code     TEXT NOT NULL,
				trade_date  TEXT NOT NULL,
				close       INTEGER NOT NULL,
				vol_lot     REAL,
				raw_close   INTEGER NOT NULL DEFAULT 0,
				PRIMARY KEY (ts_code, trade_date)
			) WITHOUT ROWID`,
			`INSERT INTO daily_bar_rebuild (ts_code, trade_date, close, vol_lot, raw_close)
				SELECT ts_code, trade_date, close, vol_lot, raw_close FROM daily_bar`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("重建步骤失败: %w", err)
			}
		}
		// 行数必须一致才允许替换：少了行说明复制出过问题，宁可回滚。
		var orig, copied int
		if err := tx.GetContext(ctx, &orig, `SELECT COUNT(*) FROM daily_bar`); err != nil {
			return err
		}
		if err := tx.GetContext(ctx, &copied, `SELECT COUNT(*) FROM daily_bar_rebuild`); err != nil {
			return err
		}
		if orig != copied {
			return fmt.Errorf("daily_bar 搬迁行数不符：原 %d 行、新表 %d 行，已回滚", orig, copied)
		}
		moved = orig
		for _, q := range []string{
			`DROP TABLE daily_bar`,
			`ALTER TABLE daily_bar_rebuild RENAME TO daily_bar`,
			`CREATE INDEX IF NOT EXISTS idx_bar_date ON daily_bar(trade_date)`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return fmt.Errorf("重建步骤失败: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("重建 daily_bar 失败: %w", err)
	}

	// 重建后回收页（表变小了，空闲页要还回去）。
	if err := IncrementalVacuum(ctx, s, 0); err != nil {
		return moved, err
	}
	if err := WALCheckpoint(ctx, s); err != nil {
		return moved, err
	}
	return moved, nil
}
