package store

import (
	"context"
	"encoding/json"
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

// TestListScreenTopNRoundTrip 候选快照的读回路径：只认 screen:topn，
// score/factors 与当日资金口径必须原样还原；上线前的日期直接缺席。
func TestListScreenTopNRoundTrip(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	snap := model.ScreenSnapshot{
		TradeDate: "20260901", TotalAssetFen: 1_000_000, SlotFen: 400_000,
		Candidates: []model.CandidateSnapshot{
			{TsCode: "600000.SH", Name: "甲", Industry: "银行", Rank: 1, PoolSize: 20,
				Score: 88.5, Momentum: 90, Value: 80, LowVol: 70, Liquidity: 60, CloseFen: 1234},
		},
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("序列化快照失败: %v", err)
	}
	if err := s.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: "20260901", Subject: model.TraceScreenTopN(),
		Outcome: model.TraceOK, Detail: string(raw), At: now,
	}); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}
	// 同前缀但不是 topn 的行不得混入。
	if err := s.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: "20260901", Subject: "screen:other",
		Outcome: model.TraceOK, Detail: `{"candidates":[{"ts_code":"WRONG"}]}`, At: now,
	}); err != nil {
		t.Fatalf("写干扰行失败: %v", err)
	}

	got, err := s.TraceRepo().ListScreenTopN(ctx, "20260901", "20260901")
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("应只读到 1 天，实际 %d 天", len(got))
	}
	g := got["20260901"]
	if g.TotalAssetFen != 1_000_000 || g.SlotFen != 400_000 || len(g.Candidates) != 1 {
		t.Fatalf("快照字段读回有误: %+v", g)
	}
	c := g.Candidates[0]
	if c.TsCode != "600000.SH" || c.Score != 88.5 {
		t.Fatalf("候选字段读回有误: %+v", c)
	}
	if c.Momentum != 90 || c.Value != 80 || c.LowVol != 70 || c.Liquidity != 60 {
		t.Errorf("因子字段读回有误: %+v", c)
	}

	empty, err := s.TraceRepo().ListScreenTopN(ctx, "20260801", "20260831")
	if err != nil || len(empty) != 0 {
		t.Errorf("无快照区间应返回空，实际 %d 天 err=%v", len(empty), err)
	}
}

// TestListLLMCodesGroupsByDate 失败候选还原：从 llm:<标的>:<prompt> 的 subject
// 取代码，按日期去重并排序；decision 行也在内（有 decision 的日期归因不会用这份数据）。
func TestListLLMCodesGroupsByDate(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	for _, c := range []model.LLMCall{
		{TradeDate: "20260901", TsCode: "600001.SH", PromptKey: "tech", Status: model.TraceOK, CreatedAt: now},
		{TradeDate: "20260901", TsCode: "600001.SH", PromptKey: "value", Status: model.TraceOK, CreatedAt: now},
		{TradeDate: "20260901", TsCode: "600002.SH", PromptKey: "sector", Status: model.TraceOK, CreatedAt: now},
		{TradeDate: "20260902", TsCode: "600003.SH", PromptKey: "decision", Status: model.TraceFail, CreatedAt: now},
	} {
		if err := s.LLMRepo().SaveCall(ctx, c); err != nil {
			t.Fatalf("写留痕失败: %v", err)
		}
	}
	got, err := s.TraceRepo().ListLLMCodes(ctx, "20260901", "20260902")
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got["20260901"]) != 2 || got["20260901"][0] != "600001.SH" || got["20260901"][1] != "600002.SH" {
		t.Errorf("20260901 代码=%v，期望 [600001.SH 600002.SH]", got["20260901"])
	}
	if len(got["20260902"]) != 1 || got["20260902"][0] != "600003.SH" {
		t.Errorf("20260902 代码=%v，期望 [600003.SH]", got["20260902"])
	}
}
