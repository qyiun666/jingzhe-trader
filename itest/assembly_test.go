package itest

import (
	"path/filepath"
	"strings"
	"testing"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

// TestAssemblyRefusesBadBaseline 装配期必须拒绝三类"跑起来也是假绿"的状态。
//
// 这三条以前都是运行期降级：本金缺失按 2 万算、收件人缺失只记一条 MAIL_NOT_CONFIGURED、
// 枚举拼错就"认不出来用默认"。现在一律在 BuildRuntime 直接失败。
// 本文件不打网络，只需要一个 token 值让 config.Load 的必配校验过。
func TestAssemblyRefusesBadBaseline(t *testing.T) {
	cases := []struct {
		name string
		seed map[string]string
		env  map[string]string // 覆盖默认环境变量（配置优先级 env > DB > 默认）
		want string
	}{
		{
			name: "本金未初始化",
			seed: map[string]string{"watch.mail_to": "me@example.com"},
			want: "本金未初始化",
		},
		{
			name: "邮件收件人为空",
			seed: map[string]string{"account.initial_capital": "20000"},
			want: "邮件配置不完整",
		},
		{
			name: "邮件被关闭",
			seed: map[string]string{"account.initial_capital": "20000", "watch.mail_to": "me@example.com"},
			env:  map[string]string{"JZ_MAIL_ENABLED": "false"},
			want: "邮件配置不完整",
		},
		{
			name: "检索档拼错",
			seed: map[string]string{"account.initial_capital": "20000", "watch.mail_to": "me@example.com",
				"llm.search_context_size": "hgieh"},
			want: "llm.search_context_size",
		},
		{
			name: "落后策略拼错",
			seed: map[string]string{"account.initial_capital": "20000", "watch.mail_to": "me@example.com",
				"goal.pace_policy": "agresiv"},
			want: "goal.pace_policy",
		},
		{
			name: "触发时刻写成 9am",
			seed: map[string]string{"account.initial_capital": "20000", "watch.mail_to": "me@example.com",
				"scheduler.pipeline": "9am"},
			want: "scheduler.pipeline",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("JZ_TUSHARE_TOKEN", "dummy-token-for-config-validation")
			t.Setenv("JZ_SERVER_API_TOKEN", "dummy-token")
			// 默认给一套完整邮件环境，让"缺失项"只由本用例自己制造。
			t.Setenv("JZ_MAIL_FROM", "from@example.com")
			t.Setenv("JZ_MAIL_PASSWORD", "auth-code")
			t.Setenv("JZ_MAIL_SMTP_HOST", "smtp.example.com")
			t.Setenv("JZ_MAIL_ENABLED", "true")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			st, err := store.Open(filepath.Join(t.TempDir(), "asm.db"))
			if err != nil {
				t.Fatalf("打开临时库失败: %v", err)
			}
			defer st.Close() //nolint:errcheck
			ctx := t.Context()
			for k, v := range tc.seed {
				if err := st.ConfigRepo().Set(ctx, k, v); err != nil {
					t.Fatalf("写 %s 失败: %v", k, err)
				}
			}
			cfg, err := config.Load(ctx, st)
			if err != nil {
				t.Fatalf("加载配置失败: %v", err)
			}
			_, err = app.BuildRuntime(ctx, st, cfg)
			if err == nil {
				t.Fatalf("装配应当失败（%s），实际成功", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误里没有 %q：实际 %v", tc.want, err)
			}
		})
	}
}

// TestAssemblyAcceptsGoodBaseline 对照组：基线齐全时必须装配成功，
// 否则上面那组"该失败的失败"就毫无意义。
func TestAssemblyAcceptsGoodBaseline(t *testing.T) {
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy-token-for-config-validation")
	t.Setenv("JZ_SERVER_API_TOKEN", "dummy-token")
	t.Setenv("JZ_MAIL_FROM", "from@example.com")
	t.Setenv("JZ_MAIL_PASSWORD", "auth-code")
	t.Setenv("JZ_MAIL_SMTP_HOST", "smtp.example.com")
	t.Setenv("JZ_MAIL_SMTP_PORT", "465")
	t.Setenv("JZ_MAIL_ENABLED", "true")
	t.Setenv("JZ_WATCH_MAIL_TO", "me@example.com")

	st, err := store.Open(filepath.Join(t.TempDir(), "asm-ok.db"))
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	defer st.Close() //nolint:errcheck
	ctx := t.Context()
	if err := st.ConfigRepo().Set(ctx, "account.initial_capital", "20000"); err != nil {
		t.Fatalf("写本金失败: %v", err)
	}
	cfg, err := config.Load(ctx, st)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	rt, err := app.BuildRuntime(ctx, st, cfg)
	if err != nil {
		t.Fatalf("基线齐全仍装配失败: %v", err)
	}
	if got := len(rt.TaskNames()); got != 5 {
		t.Errorf("注册任务数=%d，期望 5（README 与 MCP 文档都按 5 个触发点写）", got)
	}
}

// TestConfigLoadRefusesMalformedNumber 数值键写成非数字时 config.Load 必须拒绝。
//
// 这里走 st.ConfigRepo() 直写（绕过 config.Repo.Set 的键目录与类型校验），模拟库里已有的脏值：
// 手工 SQL 改的，或更早版本写进去的串。这类值过去被 GetInt 静默读成 0，
// 于是 screen.min_bar_rows 的行数门禁变成「阈值 0」，全市场同步只剩几行也照样放行。
func TestConfigLoadRefusesMalformedNumber(t *testing.T) {
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy-token-for-config-validation")
	t.Setenv("JZ_SERVER_API_TOKEN", "dummy-token")
	t.Setenv("JZ_WATCH_MAIL_TO", "me@example.com")

	for _, tc := range []struct{ key, value string }{
		{"screen.min_bar_rows", "abc"},
		{"retention.bar_days", "45天"},
		{"mail.smtp_port", "4o5"},
		{"goal.quarterly_target_pct", "15%"},
	} {
		// 该键的环境变量必须清空：env 优先级高于库值，操作者的 .env 一旦给了这个键，
		// 库里的脏值就被顶掉了，本用例测的就不再是"脏值能否通过自检"。
		t.Setenv("JZ_"+strings.ToUpper(strings.ReplaceAll(tc.key, ".", "_")), "")
		st, err := store.Open(filepath.Join(t.TempDir(), "malformed.db"))
		if err != nil {
			t.Fatalf("打开临时库失败: %v", err)
		}
		ctx := t.Context()
		if err := st.ConfigRepo().Set(ctx, tc.key, tc.value); err != nil {
			t.Fatalf("写 %s 失败: %v", tc.key, err)
		}
		_, err = config.Load(ctx, st)
		if err == nil {
			t.Errorf("%s=%q 竟然加载成功：读到业务层会静默变成零值", tc.key, tc.value)
		} else if !strings.Contains(err.Error(), tc.key) {
			t.Errorf("%s=%q 的错误没点名这个键: %v", tc.key, tc.value, err)
		} else {
			t.Log(describe("已拒绝", tc.key, "脏值", tc.value))
		}
		if cerr := st.Close(); cerr != nil {
			t.Errorf("关闭临时库失败: %v", cerr)
		}
	}
}
