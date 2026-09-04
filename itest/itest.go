// Package itest 打真实外部接口的集成测试（Tushare / DeepSeek / SMTP / gotdx / MCP HTTP）。
//
// 与单元测试的分工：internal/* 的 _test.go 全部用桩，证明逻辑自洽；
// 本文件证明"这些接口今天真的能用"——凭据缺失时一律 skip，不假装通过。
//
// 运行（凭据由启动方注入，仓库不加载 .env）：
//
//	set -a; . ./.env; set +a
//	JZ_ITEST=1 go test ./itest -run TestTushare -v
//
// 状态写入临时库，绝不碰 data/jingzhe.db；但邮件会真的发到 watch.mail_to，
// LLM 会真的计费 —— 这是这套测试唯一无法伪造的部分。
package itest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

// requireEnv 返回真实凭据下的配置与一个临时库；缺 JZ_ITEST 或必需键时 skip。
func requireEnv(t *testing.T) (*store.Store, *config.Config) {
	t.Helper()
	if os.Getenv("JZ_ITEST") != "1" {
		t.Skip("集成测试需显式开启：JZ_ITEST=1（并先注入 .env）")
	}
	// 只要求数据面凭据：MCP 令牌由测试自己生成，serve 才需要真实令牌。
	for _, k := range []string{"JZ_TUSHARE_TOKEN"} {
		if os.Getenv(k) == "" {
			t.Skipf("缺少环境变量 %s，无法打真实接口", k)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "itest.db"))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedBaseline(t, st)
	cfg, err := config.Load(t.Context(), st)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	return st, cfg
}

// seedBaseline 把"只有 jingzhe init 才会写"的两个键落到临时库：
// 本金（不配就拒绝装配）与收件人（不配就没有任何邮件）。两者都没有回落值，
// 集成测试必须显式给，否则 requireEnv 之后的每一次装配都会失败。
func seedBaseline(t *testing.T, st *store.Store) {
	t.Helper()
	capital := os.Getenv("JZ_ITEST_CAPITAL")
	if capital == "" {
		capital = "20000"
	}
	to := os.Getenv("JZ_WATCH_MAIL_TO")
	if to == "" {
		to = os.Getenv("JZ_MAIL_FROM") // 自己的告警邮箱：QQ 允许自发自收
	}
	if to == "" {
		t.Skip("JZ_WATCH_MAIL_TO / JZ_MAIL_FROM 都没有：无法验证邮件投递")
	}
	ctx := t.Context()
	for k, v := range map[string]string{"account.initial_capital": capital, "watch.mail_to": to} {
		if err := st.ConfigRepo().Set(ctx, k, v); err != nil {
			t.Fatalf("写入测试基线 %s 失败: %v", k, err)
		}
	}
}

// requireEnvOnly 只校验真实凭据是否在位（打行情/模型这类无状态接口用）。
func requireEnvOnly(t *testing.T) {
	t.Helper()
	if os.Getenv("JZ_ITEST") != "1" {
		t.Skip("集成测试需显式开启：JZ_ITEST=1（并先注入 .env）")
	}
	if os.Getenv("JZ_TUSHARE_TOKEN") == "" {
		t.Skip("缺少 JZ_TUSHARE_TOKEN")
	}
}

// requireRuntime 装配完整运行时（与 serve 同源），并校验账户基线已初始化。
func requireRuntime(t *testing.T) (*app.Runtime, *config.Config) {
	t.Helper()
	st, cfg := requireEnv(t)
	rt, err := app.BuildRuntime(t.Context(), st, cfg)
	if err != nil {
		t.Fatalf("装配运行时失败: %v", err)
	}
	return rt, cfg
}

// mailTo 把收件人写进临时库（真机 .env 不带 JZ_WATCH_MAIL_TO 时用它兜住 skip 判定）。
func mailTo(t *testing.T, cfg *config.Config) string {
	t.Helper()
	to := cfg.GetString("watch.mail_to")
	if to == "" {
		t.Skip("watch.mail_to 未配置：无法验证邮件真的投递")
	}
	return to
}

// tradeDateOr 返回最近一个已同步过日线的交易日（周末/节假日跑集成测试时的锚点）。
func tradeDateOr(t *testing.T, fallback string) string {
	t.Helper()
	if d := os.Getenv("JZ_ITEST_DATE"); d != "" {
		return d
	}
	return fallback
}

// mustNoErr 断言辅助：把 error 变成带上下文的失败。
func mustNoErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// describe 生成一行"接口返回了什么"的证据文本（写进测试日志，便于事后核对）。
//
// 参数个数为奇数时把落单的那个显式标出来：以前循环只取成对的参数，
// 尾下的值会被静默丢掉，日志里就出现"600519.SH=现价 "这种读不出结果的行。
func describe(kv ...any) string {
	out := ""
	for i := 0; i+1 < len(kv); i += 2 {
		out += fmt.Sprintf("%v=%v ", kv[i], kv[i+1])
	}
	if len(kv)%2 == 1 {
		out += fmt.Sprintf("(落单参数 %v)", kv[len(kv)-1])
	}
	return out
}
