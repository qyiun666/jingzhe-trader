package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ConfigRow config_kv 表的一行（store 层只做 CRUD，不感知业务语义）。
type ConfigRow struct {
	Key       string `db:"key"`
	Value     string `db:"value"`
	ValueType string `db:"value_type"`
	IsSecret  bool   `db:"is_secret"`
	UpdatedAt string `db:"updated_at"`
	UpdatedBy string `db:"updated_by"`
}

// ConfigRepo config_kv 读写仓储。
type ConfigRepo struct {
	db *sqlx.DB
}

// ConfigRepo 返回 config_kv 仓储（使用写池）。
func (s *Store) ConfigRepo() *ConfigRepo {
	return &ConfigRepo{db: s.writeDB}
}

// GetAll 读取全部配置行。
func (r *ConfigRepo) GetAll(ctx context.Context) ([]ConfigRow, error) {
	rows := []ConfigRow{}
	if err := r.db.SelectContext(ctx, &rows, "SELECT key, value, value_type, is_secret, updated_at, updated_by FROM config_kv ORDER BY key"); err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	return rows, nil
}

// Get 读取单个配置行。
func (r *ConfigRepo) Get(ctx context.Context, key string) (ConfigRow, error) {
	var row ConfigRow
	err := r.db.GetContext(ctx, &row, "SELECT key, value, value_type, is_secret, updated_at, updated_by FROM config_kv WHERE key = ?", key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConfigRow{}, fmt.Errorf("配置键不存在: %s", key)
		}
		return ConfigRow{}, fmt.Errorf("读取配置 %s 失败: %w", key, err)
	}
	return row, nil
}

// Upsert 写入或覆盖单个配置行（ON CONFLICT 更新值/类型/秘密标记）。
func (r *ConfigRepo) Upsert(ctx context.Context, row ConfigRow) error {
	const q = `INSERT INTO config_kv (key, value, value_type, is_secret, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			value_type = excluded.value_type,
			is_secret = excluded.is_secret,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`
	if _, err := r.db.ExecContext(ctx, q,
		row.Key, row.Value, row.ValueType, row.IsSecret, row.UpdatedAt, row.UpdatedBy,
	); err != nil {
		return fmt.Errorf("写入配置 %s 失败: %w", row.Key, err)
	}
	return nil
}
