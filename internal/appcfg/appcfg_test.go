package appcfg

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

func newTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	// 清空凭据环境变量, 否则 applyEnvOverrides 会顶掉库内值使断言失真
	t.Setenv("TUSHARE_TOKEN", "")
	t.Setenv("LLM_API_KEY", "")
	t.Setenv("JZ_API_TOKEN", "")
	t.Setenv("JZ_MAIL_PASSWORD", "")

	db, err := store.NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// sentinelDoc 按生产密钥清单反推出文档, 使测试里不出现形似凭据的字面量,
// 且清单增删时断言自动跟随
func sentinelDoc(t *testing.T, value string) []byte {
	t.Helper()
	doc := map[string]any{}
	for _, path := range config.SecretPaths() {
		section, key, found := strings.Cut(path, ".")
		if !found {
			t.Fatalf("密钥路径应为 段.键 形式: %q", path)
		}
		fields, ok := doc[section].(map[string]any)
		if !ok {
			fields = map[string]any{}
			doc[section] = fields
		}
		fields[key] = value
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("序列化测试文档失败: %v", err)
	}
	return b
}

// TestLoadSeedsDefaultsOnFirstRun 首次启动库内无 config 行时必须自动种子默认值并可读回
func TestLoadSeedsDefaultsOnFirstRun(t *testing.T) {
	db := newTestDB(t)
	repo := store.NewConfigRepo(db)

	if _, found, err := repo.Get(); err != nil || found {
		t.Fatalf("初始状态应无配置行, found=%v err=%v", found, err)
	}

	cfg, err := Load(db)
	if err != nil {
		t.Fatalf("首次 Load 失败: %v", err)
	}
	if cfg.Tushare.BaseURL != "http://api.tushare.pro" {
		t.Errorf("应拿到代码默认值, 实际 base_url=%q", cfg.Tushare.BaseURL)
	}
	if _, found, err := repo.Get(); err != nil || !found {
		t.Fatalf("种子必须已落库, found=%v err=%v", found, err)
	}
}

// TestLoadReadsStoredDocument 库内文档优先于代码默认值 (改库即改配置), 未提及的键仍回落默认
func TestLoadReadsStoredDocument(t *testing.T) {
	db := newTestDB(t)
	repo := store.NewConfigRepo(db)

	if err := repo.Put([]byte(`{"screener":{"max_pe":12.5,"enabled":true}}`)); err != nil {
		t.Fatalf("put: %v", err)
	}
	cfg, err := Load(db)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.Screener.MaxPE != 12.5 || !cfg.Screener.Enabled {
		t.Errorf("应读到库内值, 实际 max_pe=%v enabled=%v", cfg.Screener.MaxPE, cfg.Screener.Enabled)
	}
	if cfg.Screener.MaxCandidates != 20 {
		t.Errorf("文档未提及的键应回落默认值 20, 实际 %d", cfg.Screener.MaxCandidates)
	}
}

// TestEffectiveJSONMasksSecrets 脱敏只作用于 dump 通道, 运行期装载仍须拿到凭据明文
func TestEffectiveJSONMasksSecrets(t *testing.T) {
	db := newTestDB(t)
	repo := store.NewConfigRepo(db)
	doc := sentinelDoc(t, "placeholder-value-for-assert")

	if err := repo.Put(doc); err != nil {
		t.Fatalf("put: %v", err)
	}

	out, err := config.EffectiveJSON(doc)
	if err != nil {
		t.Fatalf("EffectiveJSON 失败: %v", err)
	}
	if strings.Contains(string(out), "placeholder-value-for-assert") {
		t.Errorf("脱敏输出泄漏凭据明文")
	}

	cfg, err := Load(db)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Tushare.Token != "placeholder-value-for-assert" {
		t.Errorf("运行期配置不应被脱敏影响, 实际 %q", cfg.Tushare.Token)
	}
}
