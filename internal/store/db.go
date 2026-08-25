package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动, 无需 CGO
)

// NewDB 打开 SQLite 数据库并执行建表迁移
// path 为数据库文件路径, 不存在时会自动创建
func NewDB(path string) (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 连接存活检查
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库连接失败: %w", err)
	}

	// SQLite 性能与并发优化
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",        // 写前日志, 提升并发读写
		"PRAGMA synchronous=NORMAL;",      // WAL 模式下安全且更快
		"PRAGMA busy_timeout=5000;",       // 锁等待 5 秒
		"PRAGMA foreign_keys=ON;",         // 开启外键约束
		"PRAGMA auto_vacuum=INCREMENTAL;", // 支持增量回收空间 (配合定期清理)
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %s 失败: %w", p, err)
		}
	}

	// modernc/sqlite 单写者模型: 限制单连接, 避免长期运行后偶发 SQLITE_BUSY
	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
