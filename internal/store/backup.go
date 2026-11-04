package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jmoiron/sqlx"
)

// BackupResult 一次快照的产出（CLI 与调用方都按这三项判读，不靠日志文本）。
type BackupResult struct {
	Path      string `json:"path"`      // 副本绝对路径
	Bytes     int64  `json:"bytes"`     // 副本体积
	Integrity string `json:"integrity"` // PRAGMA integrity_check 的返回值，正常是 ok
	Tables    int    `json:"tables"`    // 副本里的用户表数（与源库比对，防"截断的合法库"）
}

// DefaultBackupDir 返回快照的默认落点：<库所在目录>/backup。
//
// 路径由**库路径运行时推导**，不写死也不从命令行传——这是这个入口能在 IM 绑定会话里
// 用起来的前提：那条会话的文件沙箱只放行仓库目录，命令串里一旦出现库目录外的绝对路径
// 就被 denied_im_path_outside_session_cwd 拦下，运维就只能手工绕（11-04 那次部署正是
// 因为备份目标在沙箱外而省掉了部署前快照，见 agent_issues #15 的偏差记录）。
func DefaultBackupDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "backup")
}

// DefaultBackupPath 按时间戳给出一个不会撞车的快照文件名。
// 前缀写死 `jingzhe.pre-deploy-`：与夜间自动备份的 `auto/jingzhe-YYYYMMDD-HHMMSS.db`
// 分处不同命名空间，轮转逻辑扫不到它，部署快照不会被自动清理吃掉。
func DefaultBackupPath(dbPath string, now time.Time) string {
	return filepath.Join(DefaultBackupDir(dbPath),
		fmt.Sprintf("jingzhe.pre-deploy-%s.db", now.Format("20060102-150405")))
}

// BackupSnapshot 给现役库拍一份一致性快照，并对**副本**做完整性校验。
//
// 为什么不用 cp：库跑在 WAL 模式下，主库文件之外还有 -wal/-shm，普通复制拷到的可能是
// 「页已写、日志未合」的撕裂状态（红线：不用 cp -a 备份 WAL 库）。VACUUM INTO 是 SQLite
// 官方的在线备份通道：在同一个连接的一致性读视图里把内容写进新文件，期间不阻塞已提交的
// 后续写入，产出的是一份自洽的独立库。纯 Go 驱动（modernc）没暴露 sqlite3_backup_init
// 那套 C API，VACUUM INTO 是它在 Go 侧的等价物。
//
// 校验只信副本自己：integrity_check 逐页走一遍结构，再数一遍用户表数与源库比对——
// 光有"文件存在且能打开"不够，截断出来的库照样能开。
//
// 目标已存在直接报错：快照按时间戳命名，撞上说明同一秒内跑了两次，宁可让人看一眼，
// 也不覆盖一份已经存在的历史备份（删除历史备份需用户明确同意，这里连覆盖都不做）。
func (s *Store) BackupSnapshot(ctx context.Context, dest string) (BackupResult, error) {
	if dest == "" {
		dest = DefaultBackupPath(s.Path(), time.Now())
	}
	abs, err := filepath.Abs(dest)
	if err != nil {
		return BackupResult{}, fmt.Errorf("解析快照路径失败: %w", err)
	}
	if _, err := os.Stat(abs); err == nil {
		return BackupResult{}, fmt.Errorf("快照目标已存在，拒绝覆盖: %s（同名意味着同一秒内重复执行，请核对时间戳）", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return BackupResult{}, fmt.Errorf("检查快照目标失败 %s: %w", abs, err)
	}
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BackupResult{}, fmt.Errorf("创建快照目录 %s 失败: %w", dir, err)
	}

	// 表数先于快照取：拍完之后拿到的源表数若已变化，说明期间有 DDL，比对就没有意义。
	srcTables, err := countUserTables(ctx, s.readDB)
	if err != nil {
		return BackupResult{}, fmt.Errorf("清点源库表数失败: %w", err)
	}
	if _, err := s.writeDB.ExecContext(ctx, `VACUUM INTO ?`, abs); err != nil {
		return BackupResult{}, fmt.Errorf("生成快照失败（VACUUM INTO %s）: %w；注意 VACUUM 需要库空闲且不在事务内，避开 16:30 管线时段", abs, err)
	}
	// 库里存着 Tushare / LLM / 邮箱授权码，副本必须和源库同样收敛到 0600。
	if err := os.Chmod(abs, 0o600); err != nil {
		return BackupResult{}, fmt.Errorf("快照权限收敛为 0600 失败 %s: %w", abs, err)
	}

	fi, err := os.Stat(abs)
	if err != nil {
		return BackupResult{}, fmt.Errorf("快照写完却读不到 %s: %w", abs, err)
	}
	res := BackupResult{Path: abs, Bytes: fi.Size()}
	if res.Bytes == 0 {
		return res, fmt.Errorf("快照体积为 0（%s），判为失败", abs)
	}

	// 只读连接校验副本，绝不让校验路径有机会写这份快照。
	cdb, err := sqlx.Open("sqlite", "file:"+abs+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return res, fmt.Errorf("以只读方式打开快照失败 %s: %w", abs, err)
	}
	defer cdb.Close()
	var integ string
	if err := cdb.GetContext(ctx, &integ, `PRAGMA integrity_check`); err != nil {
		return res, fmt.Errorf("快照完整性校验失败 %s: %w", abs, err)
	}
	res.Integrity = integ
	if integ != "ok" {
		return res, fmt.Errorf("快照完整性校验不通过: %s（%s）", integ, abs)
	}
	dstTables, err := countUserTables(ctx, cdb)
	if err != nil {
		return res, fmt.Errorf("清点快照表数失败 %s: %w", abs, err)
	}
	res.Tables = dstTables
	if dstTables != srcTables {
		return res, fmt.Errorf("快照表数 %d 与源库 %d 不一致（%s），判为失败", dstTables, srcTables, abs)
	}
	return res, nil
}

// countUserTables 数用户表：排除 sqlite_ 内部表与虚拟的 sqlite_stat。
func countUserTables(ctx context.Context, db *sqlx.DB) (int, error) {
	var n int
	q := `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`
	if err := db.GetContext(ctx, &n, q); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return n, nil
}
