package config

import (
	"context"
	"errors"
	"testing"

	"jingzhe-trader/internal/store"
)

// openTestStore 打开临时库（自动建全部表），供配置测试使用。
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/cfg.db")
	if err != nil {
		t.Fatalf("store.Open 失败: %v", err)
	}
	return s
}

// TestRefuseZero 危险零值拒绝：risk.stop_loss_pct=0 应被拒绝并指明键名。
func TestRefuseZero(t *testing.T) {
	cfg := &Config{Values: map[string]ConfigValue{
		"risk.stop_loss_pct": {Key: "risk.stop_loss_pct", Type: TypeFloat, Value: "0"},
	}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("期望拒绝零值，但 Validate 通过")
	}
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 *ConfigError，实际 %T", err)
	}
	found := false
	for _, k := range ce.ZeroValues {
		if k == "risk.stop_loss_pct" {
			found = true
		}
	}
	if !found {
		t.Fatalf("零值清单应含 risk.stop_loss_pct，实际: %v", ce.ZeroValues)
	}
}

// TestRequiredMissing 必配项为空拒绝：tushare.token / server.api_token 缺失应列出。
func TestRequiredMissing(t *testing.T) {
	cfg := &Config{Values: map[string]ConfigValue{}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("期望拒绝缺失必配项，但 Validate 通过")
	}
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("期望 *ConfigError，实际 %T", err)
	}
	has := func(list []string, k string) bool {
		for _, x := range list {
			if x == k {
				return true
			}
		}
		return false
	}
	if !has(ce.Missing, "tushare.token") || !has(ce.Missing, "server.api_token") {
		t.Fatalf("缺失清单应含 tushare.token 与 server.api_token，实际: %v", ce.Missing)
	}
}

// TestEnvOverridePriority 环境变量 > 库值 > 默认值（仅非空顶换）。
func TestEnvOverridePriority(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	// 库值：screen.top_n = 5（默认值 20）
	repo := NewRepo(s)
	if err := repo.Set(ctx, "screen.top_n", "5", "test"); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	// 环境变量顶换：JZ_SCREEN_TOP_N = 7
	t.Setenv("JZ_SCREEN_TOP_N", "7")
	// Load 校验必配项，提供凭据占位（envName: tushare.token→JZ_TUSHARE_TOKEN, server.api_token→JZ_SERVER_API_TOKEN）
	t.Setenv("JZ_TUSHARE_TOKEN", "env_tushare")
	t.Setenv("JZ_SERVER_API_TOKEN", "env_api")

	cfg, err := Load(ctx, s)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := cfg.GetInt("screen.top_n"); got != 7 {
		t.Fatalf("环境变量应顶换库值：期望 7，实际 %d", got)
	}

	// 默认值基线：未设置的 risk.stop_loss_pct 应取默认 0.08
	if got := cfg.GetFloat("risk.stop_loss_pct"); got != 0.08 {
		t.Fatalf("未设置键应取默认：期望 0.08，实际 %v", got)
	}
}

// TestEnvSecretOverride 凭据环境变量覆盖生效（JZ_SERVER_API_TOKEN）。
func TestEnvSecretOverride(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	t.Setenv("JZ_TUSHARE_TOKEN", "env_tushare")
	t.Setenv("JZ_SERVER_API_TOKEN", "env_api")

	cfg, err := Load(ctx, s)
	if err != nil {
		t.Fatalf("Load 失败（凭据由 env 提供）: %v", err)
	}
	if cfg.GetString("tushare.token") != "env_tushare" {
		t.Fatalf("tushare.token 应取 env 值")
	}
	if cfg.GetString("server.api_token") != "env_api" {
		t.Fatalf("server.api_token 应取 env 值")
	}
}

// TestDBOverridesDefault 库值覆盖默认值。
func TestDBOverridesDefault(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	repo := NewRepo(s)
	if err := repo.Set(ctx, "screen.top_n", "5", "test"); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	// 不设业务 env；Load 校验必配项，提供凭据占位
	t.Setenv("JZ_TUSHARE_TOKEN", "env_tushare")
	t.Setenv("JZ_SERVER_API_TOKEN", "env_api")
	cfg, err := Load(ctx, s)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := cfg.GetInt("screen.top_n"); got != 5 {
		t.Fatalf("库值应覆盖默认：期望 5，实际 %d", got)
	}
}

// TestMaskNotLeak 凭据掩码不泄露明文。
func TestMaskNotLeak(t *testing.T) {
	if Mask("abcdef") != maskLiteral {
		t.Fatal("Mask 应返回掩码")
	}
	if Mask("") != "" {
		t.Fatal("空值掩码应返回空")
	}
	e := Entry{Key: "tushare.token", Value: "supersecret", IsSecret: true}
	if DisplayValue(e, false) != maskLiteral {
		t.Fatal("未显式 show-secrets 时凭据应掩码")
	}
	if DisplayValue(e, true) != "supersecret" {
		t.Fatal("显式 show-secrets 应返回明文")
	}
	// 非凭据键不掩码
	ne := Entry{Key: "risk.stop_loss_pct", Value: "0.08", IsSecret: false}
	if DisplayValue(ne, false) != "0.08" {
		t.Fatal("非凭据键不应掩码")
	}
}
