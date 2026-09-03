package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// CurrentSchemaVersion 当前 schema 版本（Batch 1 = 1，含全部 28 张表）。
const CurrentSchemaVersion = 1

// schemaVersionDDL 版本记录表。
const schemaVersionDDL = `CREATE TABLE IF NOT EXISTS schema_version (
	version    INTEGER NOT NULL,
	applied_at TEXT NOT NULL
)`

// Migrate 执行版本迁移：确保 schema_version 表存在，按 from→to 顺序应用未执行的迁移脚本。
// 每版一个 func，保证后续批次可安全追加（Batch 2+ 在此注册 migration v2...）。
func Migrate(s *Store) error {
	if _, err := s.writeDB.Exec(schemaVersionDDL); err != nil {
		return fmt.Errorf("建 schema_version 表失败: %w", err)
	}

	var v int
	err := s.writeDB.Get(&v, "SELECT version FROM schema_version ORDER BY version DESC LIMIT 1")
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("读取 schema_version 失败: %w", err)
		}
		v = 0
	}

	// 迁移脚本表：顺序即版本号。
	migrations := []func(*sqlx.DB) error{
		nil,        // index 0 占位（版本从 1 开始）
		applyV1,    // v1：全部 28 张表
	}

	for to := v + 1; to < len(migrations); to++ {
		fn := migrations[to]
		if fn == nil {
			continue
		}
		if err := fn(s.writeDB); err != nil {
			return fmt.Errorf("迁移到 v%d 失败: %w", to, err)
		}
		if _, err := s.writeDB.Exec(
			"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
			to, time.Now().UTC().Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("写入 schema_version v%d 失败: %w", to, err)
		}
	}
	return nil
}

// applyV1 建全部 28 张表（幂等 IF NOT EXISTS）。
func applyV1(db *sqlx.DB) error {
	return CreateTables(db)
}
