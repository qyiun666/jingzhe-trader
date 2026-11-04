package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// TestBackupSnapshotDefaultTarget 锁 #15 偏差项的正面产出：入口能在 IM 绑定会话里
// 被调起来，靠的就是"目标路径由库路径运行时推导、命令串里不出现库目录外的绝对路径"。
func TestBackupSnapshotDefaultTarget(t *testing.T) {
	st := openStoreAtPath(t, filepath.Join(t.TempDir(), "jingzhe.db"))
	defer st.Close()

	rel := DefaultBackupPath(st.Path(), fixedBackupTime())
	if got, want := filepath.Dir(rel), filepath.Join(filepath.Dir(st.Path()), "backup"); got != want {
		t.Fatalf("默认快照目录 = %s, 期望 %s", got, want)
	}
	if got, want := filepath.Base(rel), "jingzhe.pre-deploy-20261105-111403.db"; got != want {
		t.Fatalf("快照文件名 = %s, 期望 %s（前缀与夜间 auto/ 分开命名空间，轮转扫不到它）", got, want)
	}
}

func TestBackupSnapshotMakesVerifiedCopy(t *testing.T) {
	st := openStoreAtPath(t, filepath.Join(t.TempDir(), "jingzhe.db"))
	defer st.Close()
	ctx := context.Background()

	probe := "600704.SH"
	if _, err := st.writeDB.ExecContext(ctx,
		`INSERT INTO config_kv(key,value) VALUES(?,?)`, "backup.probe", probe); err != nil {
		t.Fatalf("写入探针行失败: %v", err)
	}

	res, err := st.BackupSnapshot(ctx, "")
	if err != nil {
		t.Fatalf("BackupSnapshot 失败: %v", err)
	}
	if res.Integrity != "ok" {
		t.Errorf("integrity=%s, 期望 ok", res.Integrity)
	}
	if res.Bytes <= 0 {
		t.Errorf("快照体积=%d, 期望 >0", res.Bytes)
	}
	// 现役 schema 共 7 张用户表；比对表数是为了拦住"能打开但被截断"的副本。
	if res.Tables != 7 {
		t.Errorf("快照用户表数=%d, 期望 7", res.Tables)
	}
	fi, err := os.Stat(res.Path)
	if err != nil {
		t.Fatalf("快照文件不可读: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("快照权限=%o, 期望 600（库内含凭据）", perm)
	}

	// 副本里必须读得到探针行——只校验 integrity 不够，得证明数据真进去了。
	cdb, err := sqlx.Open("sqlite", "file:"+res.Path+"?mode=ro")
	if err != nil {
		t.Fatalf("只读打开快照失败: %v", err)
	}
	defer cdb.Close()
	var val string
	if err := cdb.Get(&val, `SELECT value FROM config_kv WHERE key='backup.probe'`); err != nil {
		t.Fatalf("快照里读不到探针行: %v", err)
	}
	if val != probe {
		t.Errorf("快照里的值=%s, 期望 %s", val, probe)
	}

	// 拍快照不许动源库：源库照常可读、体积不为 0、仍在那一行上。
	var after string
	if err := st.readDB.GetContext(ctx, &after, `SELECT value FROM config_kv WHERE key='backup.probe'`); err != nil {
		t.Fatalf("快照后源库读不到探针行: %v", err)
	}
	if src, err := os.Stat(st.Path()); err != nil || src.Size() <= 0 {
		t.Errorf("源库状态异常: %v err=%v", src, err)
	}
}

func TestBackupSnapshotRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	st := openStoreAtPath(t, filepath.Join(dir, "jingzhe.db"))
	defer st.Close()

	dest := filepath.Join(dir, "backup", "jingzhe.pre-deploy-existing.db")
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(dest, []byte("not-empty"), 0o600); err != nil {
		t.Fatalf("放置同名文件失败: %v", err)
	}

	// 已存在的历史备份一律不覆盖（删除需用户明确同意，覆盖同样不该悄悄发生）。
	_, err := st.BackupSnapshot(context.Background(), dest)
	if err == nil || !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("目标已存在时竟然成功了, 实际 err=%v", err)
	}
	orig, _ := os.ReadFile(dest)
	if string(orig) != "not-empty" {
		t.Errorf("已存在的备份被改写了: %q", orig)
	}
}

func TestBackupSnapshotExplicitTarget(t *testing.T) {
	dir := t.TempDir()
	st := openStoreAtPath(t, filepath.Join(dir, "jingzhe.db"))
	defer st.Close()

	dest := filepath.Join(dir, "elsewhere", "pre-check.db")
	res, err := st.BackupSnapshot(context.Background(), dest)
	if err != nil {
		t.Fatalf("指定目标拍快照失败: %v", err)
	}
	if res.Path != dest {
		t.Errorf("快照落在 %s, 期望 %s", res.Path, dest)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("目标目录应被自动创建: %v", err)
	}
}

// fixedBackupTime 给命名规则一个可比对的固定时刻。
func fixedBackupTime() time.Time {
	return time.Date(2026, 11, 5, 11, 14, 3, 0, time.Local)
}
