package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ConfigKVKey config_kv 内存放整棵应用配置文档的键
// 配置只占这一行, 与 cash/initial_capital 等运行态键同表不同键
const ConfigKVKey = "config"

// ConfigRepo 应用配置文档仓储 (整棵配置以 JSON 文档存于 config_kv 单行)
type ConfigRepo struct {
	db *sqlx.DB
}

// NewConfigRepo 构造 ConfigRepo
func NewConfigRepo(db *sqlx.DB) *ConfigRepo {
	return &ConfigRepo{db: db}
}

// Get 读取配置文档 JSON; found=false 表示尚未写入 (首次启动, 由调用方种子)
func (r *ConfigRepo) Get() (data []byte, found bool, err error) {
	var value string
	err = r.db.Get(&value, `SELECT value FROM config_kv WHERE key = ?`, ConfigKVKey)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("读取配置文档失败: %w", err)
	}
	return []byte(value), true, nil
}

// Put 整行覆盖写入配置文档
// 文档内含凭据: 本方法及其错误信息不得携带正文, 失败只报键名
func (r *ConfigRepo) Put(data []byte) error {
	_, err := r.db.Exec(
		`INSERT INTO config_kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		ConfigKVKey, string(data), time.Now().Format(TimeLayout),
	)
	if err != nil {
		return fmt.Errorf("写入配置文档失败(key=%s): %w", ConfigKVKey, err)
	}
	return nil
}
