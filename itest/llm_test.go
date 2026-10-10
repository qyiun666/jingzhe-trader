package itest

import (
	"context"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/llm"
)

// requireCfg 只关心外部接口，不碰库：requireEnv 建的临时库随测试自动关闭。
func requireCfg(t *testing.T) *config.Config {
	t.Helper()
	_, cfg := requireEnv(t)
	return cfg
}

// TestLLMComplete 一次最小补全：证明 key、模型名、/v1/responses 协议三件事同时成立。
func TestLLMComplete(t *testing.T) {
	cfg := requireCfg(t)
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	cli := llm.NewClient(cfg.GetString("llm.api_key"), cfg.GetString("llm.base_url"),
		cfg.GetString("llm.model"), cfg.GetString("llm.search_context_size"), nil)
	if !cli.Enabled() {
		t.Skip("llm.api_key / llm.model 未配置")
	}
	out, err := cli.Complete(ctx, llm.Request{
		System: "只输出一个 JSON 对象，不要任何解释。",
		User:   `给出 {"ok":true} 这个对象。`,
	})
	mustNoErr(t, "LLM 补全", err)
	if !strings.Contains(out, "\"ok\"") {
		t.Fatalf("模型没有按约束输出 JSON，实际: %q", out)
	}
	t.Log(describe("model", cfg.GetString("llm.model"), "回复", strings.TrimSpace(out)))
}

// 这里原来有一个 TestLLMWebSearch：挂 web_search 问一次，答出 JSON 就算绿。
// 2026-10-09 实测证明它是假绿 —— 这个账号/模型组合下 /v1/responses 从来没执行过
// web_search（5 种请求形态 × 3 档检索深度，响应里没有一个 web_search_call item），
// 模型只有两种收尾：把工具调用当正文吐出来（<｜｜DSML｜｜ invoke name="web_search"），
// 或者干脆编一句"检索未见风险公告"。前者现在被 client.go 的 looksLikeToolCallLeak
// 判成 errNoAnswer 重试，后者会直接进决策 —— 比失败更危险。
// 所以评审链已全面摘掉检索（见 internal/llm/prompts.go 顶部），这个用例随之删除：
// 它验证的能力不存在，留着只会让人重新相信"消息面有外部事实"。
