package store

import (
	"context"
	"testing"
	"time"

	"jingzhe-trader/internal/model"
)

// TestListLLMDecisionsRoundTrip 决策留痕的读回路径。
//
// 存在的理由：store 不许 import llm（依赖方向铁律），所以决策 prompt 的键在
// repo_trace.go 里是字面量 "decision"，与 llm.KeyDecision 是刻意的双真相。
// 这条测试把它锚在 llm.KeyDecision 的实际取值上——两边哪天改散，这里就红。
func TestListLLMDecisionsRoundTrip(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	// 决策行 + 证据行：只有决策行该被 ListLLMDecisions 读到。
	if err := s.LLMRepo().SaveCall(ctx, model.LLMCall{
		TradeDate: "20260901", TsCode: "600000.SH", PromptKey: "decision",
		Verdict: "buy", Confidence: 0.8, WeightPct: 0.3, Rationale: "理由", Status: model.TraceOK, CreatedAt: now,
	}); err != nil {
		t.Fatalf("写决策行失败: %v", err)
	}
	if err := s.LLMRepo().SaveCall(ctx, model.LLMCall{
		TradeDate: "20260901", TsCode: "600000.SH", PromptKey: "tech",
		Verdict: "positive", Status: model.TraceOK, CreatedAt: now,
	}); err != nil {
		t.Fatalf("写证据行失败: %v", err)
	}

	got, err := s.TraceRepo().ListLLMDecisions(ctx, "20260901", "20260901")
	if err != nil {
		t.Fatalf("读取决策留痕失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应只读到 1 条决策行，实际 %d 条", len(got))
	}
	d := got[0]
	if d.TsCode != "600000.SH" || d.Verdict != "buy" || d.Confidence != 0.8 || d.WeightPct != 0.3 {
		t.Errorf("决策行字段读回有误: %+v", d)
	}
	if d.PromptKey != llmPromptKeyDecision {
		t.Errorf("PromptKey = %q，期望 %q", d.PromptKey, llmPromptKeyDecision)
	}
}

// TestListLLMDecisionsFiltersByDate 归因按日期区间取行，区间外不得混入。
func TestListLLMDecisionsFiltersByDate(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, d := range []string{"20260825", "20260901", "20260902"} {
		if err := s.LLMRepo().SaveCall(ctx, model.LLMCall{
			TradeDate: d, TsCode: "600000.SH", PromptKey: "decision",
			Verdict: "buy", Status: model.TraceOK, CreatedAt: now,
		}); err != nil {
			t.Fatalf("写决策行 %s 失败: %v", d, err)
		}
	}
	got, err := s.TraceRepo().ListLLMDecisions(ctx, "20260901", "20260902")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("区间内应 2 条，实际 %d 条", len(got))
	}
	for _, d := range got {
		if d.TradeDate < "20260901" || d.TradeDate > "20260902" {
			t.Errorf("区间外的行被读入: %s", d.TradeDate)
		}
	}
}
