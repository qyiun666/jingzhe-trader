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

// TestLLMWebSearch 挂 web_search 的那一条维度：按**配置的检索档**问一次就必须答出来。
//
// 以前这里写着"high 档问不出结果就降到 low 再问一次"，实测那不是档位的问题，
// 是请求从来没传 max_output_tokens：默认输出预算被 reasoning 吃满，
// 整批 JSON 写到一半就停了。预算修好之后，降档重试没有任何存在理由，已删。
// 所以这个用例只问一次、只断言一次 —— 它现在的职责是守住"配置档能用"。
func TestLLMWebSearch(t *testing.T) {
	cfg := requireCfg(t)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Second)
	defer cancel()

	wanted := cfg.GetString("llm.search_context_size")
	cli := llm.NewClient(cfg.GetString("llm.api_key"), cfg.GetString("llm.base_url"),
		cfg.GetString("llm.model"), wanted, nil)
	if !cli.Enabled() {
		t.Skip("llm.api_key / llm.model 未配置")
	}
	out, err := cli.Complete(ctx, llm.Request{
		System: "联网查证后只输出一个 JSON 对象，不要解释。",
		User:   `查沪深300指数最近一个交易日的收盘点位，只输出 {"hit":true}`,
		Search: true,
	})
	if err != nil {
		t.Fatalf("配置档 %q 问一次就失败: %v", wanted, err)
	}
	if !strings.Contains(out, `"hit"`) {
		t.Fatalf("配置档 %q 没答出契约要求的 JSON: %q", wanted, out)
	}
	t.Log(describe("检索档", wanted, "回复", strings.TrimSpace(out)))
}
