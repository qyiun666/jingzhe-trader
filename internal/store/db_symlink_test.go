package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件锁 #15①：Open 必须把 `-db` 上的符号链接摘成真身再做后续判断。
//
// 11-03 那次常驻服务被拉成 `-db .qoder-collab-runtime/jingzhe.db`（平台托管的协作运行时目录里
// 临时搭的链接）。两个后果都是真的：进程依赖链接存活，目录被清理后自愈/人工重启一律
// "打开数据库失败"；而 checkNoSyncMarkers 拿链接目录去查同步标记，真库若住在同步目录里
// 就永远看不见 .stfolder —— D6 那条"库不得住在同步目录"的防线静默失效。

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("建符号链接 %s -> %s 失败: %v", link, target, err)
	}
}

// canonical 把期望值也解析一次：t.TempDir() 所在前缀（如 /tmp）本身可能是链接。
func canonical(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("解析期望路径 %s 失败: %v", p, err)
	}
	return r
}

func TestOpenResolvesSymlinkedDBPath(t *testing.T) {
	realDir, linkDir := t.TempDir(), t.TempDir()
	realDB := filepath.Join(realDir, "jingzhe.db")
	linkDB := filepath.Join(linkDir, "jingzhe.db")

	s := openStoreAtPath(t, realDB)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭临时库失败: %v", err)
	}
	mustSymlink(t, realDB, linkDB)

	st := openStoreAtPath(t, linkDB)
	defer st.Close()

	if want := canonical(t, realDB); st.Path() != want {
		t.Errorf("Store.Path() = %s, 期望真身 %s", st.Path(), want)
	}
	if strings.Contains(st.Path(), linkDir) {
		t.Errorf("Path 里还带着链接目录 %s：进程仍在依赖链接存活", linkDir)
	}
}

func TestOpenRejectsSyncMarkerInRealDirBehindSymlink(t *testing.T) {
	realDir, linkDir := t.TempDir(), t.TempDir()
	realDB := filepath.Join(realDir, "jingzhe.db")

	s := openStoreAtPath(t, realDB)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭临时库失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, ".stignore"), []byte("*.db\n"), 0o600); err != nil {
		t.Fatalf("写同步标记失败: %v", err)
	}
	linkDB := filepath.Join(linkDir, "jingzhe.db")
	mustSymlink(t, realDB, linkDB)

	// 链接目录干干净净、真库目录住着同步标记：按真身判必须拒开。
	if _, err := Open(linkDB); err == nil {
		t.Fatal("真库目录里的同步标记没拦住——守卫顺着链接查了链接目录，D6 防线被绕过")
	} else if !strings.Contains(err.Error(), ".stignore") {
		t.Errorf("拒绝原因应点名同步标记，实际: %v", err)
	}
}

func TestOpenIgnoresSyncMarkerBesideSymlink(t *testing.T) {
	realDir, linkDir := t.TempDir(), t.TempDir()
	realDB := filepath.Join(realDir, "jingzhe.db")

	s := openStoreAtPath(t, realDB)
	if err := s.Close(); err != nil {
		t.Fatalf("关闭临时库失败: %v", err)
	}
	// 标记只落在链接所在目录（协作运行时目录那类）：它不是库的邻居，不该拦启动。
	// 这条守的是 #10 那类可用性教训——守卫扩大化会把"可能被覆盖"变成"起不来"。
	if err := os.WriteFile(filepath.Join(linkDir, ".stignore"), []byte("*.db\n"), 0o600); err != nil {
		t.Fatalf("写同步标记失败: %v", err)
	}
	linkDB := filepath.Join(linkDir, "jingzhe.db")
	mustSymlink(t, realDB, linkDB)

	st := openStoreAtPath(t, linkDB)
	defer st.Close()
	if want := canonical(t, realDB); st.Path() != want {
		t.Errorf("Path 应为真身 %s, 实际 %s", want, st.Path())
	}
}

func TestResolveDBPathNewFileUnderSymlinkedDir(t *testing.T) {
	realDir := t.TempDir()
	mountDir := filepath.Join(t.TempDir(), "mnt")
	mustSymlink(t, realDir, mountDir)

	// 库还没建（首次建库）：EvalSymlinks 对文件报 ENOENT，必须退一步解析父目录，
	// 否则守卫查的还是链接目录。
	target := filepath.Join(mountDir, "fresh.db")
	got, err := resolveDBPath(target)
	if err != nil {
		t.Fatalf("resolveDBPath(%s) 失败: %v", target, err)
	}
	if want := filepath.Join(canonical(t, realDir), "fresh.db"); got != want {
		t.Errorf("解析结果 %s, 期望 %s", got, want)
	}

	// 同一条路径上，真目录里的同步标记要在 Open 这一步就拦住（文件还不存在也要拦）。
	if err := os.WriteFile(filepath.Join(realDir, ".stfolder"), nil, 0o600); err != nil {
		t.Fatalf("写同步标记失败: %v", err)
	}
	if _, err := Open(target); err == nil || !strings.Contains(err.Error(), ".stfolder") {
		t.Fatalf("新建库走链接目录时没查真目录的标记, 实际 err=%v", err)
	}
}
