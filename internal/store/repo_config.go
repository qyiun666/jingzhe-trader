package store

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// ConfigRepo config_kv 读写仓储。
//
// 这张表就两列：key → value。类型、是否凭据、默认值都来自代码里的键目录
// （config.KeySpecs），不在库里再存一份镜像 —— 那份镜像从来没被读过。
// 谁在什么时候改了配置，写服务日志，不占列。
//
// 除配置键外，库里还有三类机器写入的状态键：goal.state、suspend:<YYYYMMDD>、
// account.cash_anchor*。它们不进键目录，所以 config set 拒绝写、config dump 不显示。
type ConfigRepo struct {
	db *sqlx.DB
}

// ConfigRepo 返回 config_kv 仓储（使用写池）。
func (s *Store) ConfigRepo() *ConfigRepo {
	return &ConfigRepo{db: s.writeDB}
}

// GetAll 读取全部键值（配置 + 状态键一起返回，调用方按键取自己那部分）。
func (r *ConfigRepo) GetAll(ctx context.Context) (map[string]string, error) {
	var rows []struct {
		Key   string `db:"key"`
		Value string `db:"value"`
	}
	if err := r.db.SelectContext(ctx, &rows, "SELECT key, value FROM config_kv ORDER BY key"); err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	m := make(map[string]string, len(rows))
	for _, row := range rows {
		m[row.Key] = row.Value
	}
	return m, nil
}

// Set 写入或覆盖单个键。
func (r *ConfigRepo) Set(ctx context.Context, key, value string) error {
	const q = `INSERT INTO config_kv (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	if _, err := r.db.ExecContext(ctx, q, key, value); err != nil {
		return fmt.Errorf("写入配置 %s 失败: %w", key, err)
	}
	return nil
}
