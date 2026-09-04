// Package store SQLite 单一数据源：连接/建表/迁移/仓储/保留清理。
//
// 依赖方向（ARCHITECTURE §1.1）：store 只依赖 model，禁止 import 任何业务包。
// 外部 IO 只在适配层，store 层禁止 net/http。
package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，无 CGO（硬约束）
)

// Store 封装单写者写池 + 独立读池（ARCHITECTURE §11.5）。
type Store struct {
	writeDB *sqlx.DB
	readDB  *sqlx.DB
	path    string
}

// syncMarkers 文件同步标记：命中则拒绝启动，防止库文件被 Syncthing/云盘同步破坏（D6）。
var syncMarkers = []string{".stfolder", ".stversions", ".stignore", ".syncthing", "CloudStation"}

// Open 打开（或创建）SQLite 数据库并完成全部启动自检：
//  1. 解析路径并创建目录
//  2. 检测目录是否存在文件同步标记（命中拒绝启动）
//  3. 应用 PRAGMA（WAL / busy_timeout / auto_vacuum=INCREMENTAL / synchronous=NORMAL / foreign_keys）
//  4. 校验 PRAGMA 实际生效
//  5. 执行建表迁移
func Open(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析数据库路径失败: %w", err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据库目录 %s 失败: %w", dir, err)
	}
	if err := checkNoSyncMarkers(dir); err != nil {
		return nil, err
	}

	dsn := buildDSN(abs)

	wdb, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开写库失败: %w", err)
	}
	wdb.SetMaxOpenConns(1) // 单写者（§3.0 / §11.5）
	if err := wdb.Ping(); err != nil {
		wdb.Close()
		return nil, fmt.Errorf("写库连接失败: %w", err)
	}

	rdb, err := sqlx.Open("sqlite", dsn)
	if err != nil {
		wdb.Close()
		return nil, fmt.Errorf("打开读库失败: %w", err)
	}
	rdb.SetMaxOpenConns(10) // 读池独立
	if err := rdb.Ping(); err != nil {
		wdb.Close()
		rdb.Close()
		return nil, fmt.Errorf("读库连接失败: %w", err)
	}

	st := &Store{writeDB: wdb, readDB: rdb, path: abs}

	// 库文件权限收敛（§6.3）：库里存着 Tushare / LLM / 邮箱授权码，锁不住就不开。
	// store 层禁止 import 业务包（含 observability），所以这里只能上抛而不是记日志。
	if err := os.Chmod(abs, 0o600); err != nil {
		wdb.Close()
		rdb.Close()
		return nil, fmt.Errorf("库文件 %s 权限收敛为 0600 失败（内含凭据，拒绝打开）: %w", abs, err)
	}

	if err := st.VerifyPragmas(context.Background()); err != nil {
		wdb.Close()
		rdb.Close()
		return nil, fmt.Errorf("数据库 PRAGMA 校验失败: %w", err)
	}
	if err := CreateTables(st.writeDB); err != nil {
		wdb.Close()
		rdb.Close()
		return nil, fmt.Errorf("数据库建表失败: %w", err)
	}
	return st, nil
}

// buildDSN 构造带 PRAGMA 的 DSN。modernc.org/sqlite 的 _pragma 参数对连接池内每个新连接生效。
func buildDSN(path string) string {
	return "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=auto_vacuum(INCREMENTAL)"
}

// checkNoSyncMarkers 检测库目录是否存在文件同步标记（D6 / P0-15-6）。
func checkNoSyncMarkers(dir string) error {
	for _, m := range syncMarkers {
		p := filepath.Join(dir, m)
		if fi, err := os.Stat(p); err == nil && fi != nil {
			return fmt.Errorf("数据库目录 %s 存在文件同步标记 %s，拒绝启动以防数据被同步破坏（如需使用请将库移出同步目录）", dir, m)
		}
	}
	return nil
}

// WriteDB 返回单写者写连接池。
func (s *Store) WriteDB() *sqlx.DB { return s.writeDB }

// ReadDB 返回独立读连接池。
func (s *Store) ReadDB() *sqlx.DB { return s.readDB }

// Path 返回数据库绝对路径。
func (s *Store) Path() string { return s.path }

// Close 关闭读写连接池。
func (s *Store) Close() error {
	var firstErr error
	if err := s.readDB.Close(); err != nil {
		firstErr = err
	}
	if err := s.writeDB.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// PragmaInfo 返回当前 PRAGMA 实际值，用于启动自检报告（验收 #5）。
type PragmaInfo struct {
	JournalMode string
	AutoVacuum  int
	BusyTimeout int
}

// VerifyPragmas 校验 PRAGMA 实际生效：journal_mode=wal、auto_vacuum=2、busy_timeout≥5000。
func (s *Store) VerifyPragmas(ctx context.Context) error {
	var jm string
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&jm); err != nil {
		return fmt.Errorf("读取 journal_mode 失败: %w", err)
	}
	if jm != "wal" {
		return fmt.Errorf("journal_mode=%s, 期望 wal", jm)
	}
	var av int
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&av); err != nil {
		return fmt.Errorf("读取 auto_vacuum 失败: %w", err)
	}
	if av != 2 {
		return fmt.Errorf("auto_vacuum=%d, 期望 2(INCREMENTAL)；旧库需离线 VACUUM 重建", av)
	}
	var bt int
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
		return fmt.Errorf("读取 busy_timeout 失败: %w", err)
	}
	if bt < 5000 {
		return fmt.Errorf("busy_timeout=%d, 期望 ≥5000", bt)
	}
	return nil
}

// PragmaInfo 返回当前 PRAGMA 实际值。
func (s *Store) PragmaInfo(ctx context.Context) (PragmaInfo, error) {
	var info PragmaInfo
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&info.JournalMode); err != nil {
		return info, fmt.Errorf("读取 journal_mode 失败: %w", err)
	}
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&info.AutoVacuum); err != nil {
		return info, fmt.Errorf("读取 auto_vacuum 失败: %w", err)
	}
	if err := s.writeDB.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&info.BusyTimeout); err != nil {
		return info, fmt.Errorf("读取 busy_timeout 失败: %w", err)
	}
	return info, nil
}
