// Package appcfg 组装"配置来自 SQLite"的启动链路
//
// 独立成包的原因: 配置声明在 internal/config (纯结构体+默认值), 数据读写在 internal/store,
// 两者都不该引用对方。引导逻辑需要同时用到它们, 放在任一 cmd 里又会重复五遍,
// 因此由本包承担组合根职责 (先开库 → 再读配置)。
package appcfg

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

// EnvDBPath 覆盖默认库路径的环境变量 (部署时无需任何配置文件即可改库位置)
const EnvDBPath = "JZ_DB_PATH"

// ResolveDBPath 按 显式入参 > JZ_DB_PATH > config.DefaultDBPath() 决定数据库路径。
// flagValue 传空字符串表示用户未显式指定, 才轮到环境变量。
func ResolveDBPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if p := os.Getenv(EnvDBPath); p != "" {
		return p
	}
	return config.DefaultDBPath()
}

// Open 建好数据目录后打开 SQLite (内部完成建表迁移)
func Open(path string) (*sqlx.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}
	db, err := store.NewDB(path)
	if err != nil {
		return nil, err
	}
	return db, nil
}

// Load 从库内读取应用配置; 尚无配置行时以代码默认值种子写入, 保证首次启动即可运行。
// 库路径本身不可能存在库里, 故由 ResolveDBPath 决定, 本函数只读不猜。
func Load(db *sqlx.DB) (*config.Config, error) {
	repo := store.NewConfigRepo(db)
	data, found, err := repo.Get()
	if err != nil {
		return nil, err
	}
	if !found {
		data, err = config.DefaultJSON()
		if err != nil {
			return nil, err
		}
		if err := repo.Put(data); err != nil {
			return nil, fmt.Errorf("种子默认配置失败: %w", err)
		}
	}
	cfg, err := config.LoadFromJSON(data)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
