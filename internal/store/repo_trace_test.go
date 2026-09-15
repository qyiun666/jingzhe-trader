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

// TestCalibrationRoundTrip 归因行写回读必须保真，且与 llm: 行互不覆盖（不同命名空间）。
func TestCalibrationRoundTrip(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	if err := s.TraceRepo().WriteCalibration(ctx, model.Calibration{
		TradeDate: "20260901", TsCode: "600000.SH", Verdict: "buy",
		Confidence: 0.8, WeightPct: 0.3, Ret5: 0.02, Has5: true, Ret20: -0.03, Has20: true, At: now,
	}); err != nil {
		t.Fatalf("写归因行失败: %v", err)
	}
	// 同一标的同一日的决策留痕：两个 subject 不同，不得互相覆盖。
	if err := s.LLMRepo().SaveCall(ctx, model.LLMCall{
		TradeDate: "20260901", TsCode: "600000.SH", PromptKey: "decision",
		Verdict: "buy", Confidence: 0.8, Status: model.TraceOK, CreatedAt: now,
	}); err != nil {
		t.Fatalf("写决策行失败: %v", err)
	}

	rows, err := s.TraceRepo().ListCalibrations(ctx, "20260901", "20260901")
	if err != nil {
		t.Fatalf("读取归因失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应 1 条归因行，实际 %d", len(rows))
	}
	r := rows[0]
	if !r.Has5 || r.Ret5 != 0.02 || !r.Has20 || r.Ret20 != -0.03 || r.Confidence != 0.8 {
		t.Errorf("归因行读回有误: %+v", r)
	}
	// 决策行仍在（没被 cal: 行顶掉）。
	dec, err := s.TraceRepo().ListLLMDecisions(ctx, "20260901", "20260901")
	if err != nil {
		t.Fatalf("读取决策行失败: %v", err)
	}
	if len(dec) != 1 {
		t.Fatalf("决策行被归因行覆盖了：%d 条", len(dec))
	}

	// 重跑覆盖：同一 (日, 标的) 再写一次不新增行。
	if err := s.TraceRepo().WriteCalibration(ctx, model.Calibration{
		TradeDate: "20260901", TsCode: "600000.SH", Verdict: "buy", Confidence: 0.8, At: now,
	}); err != nil {
		t.Fatalf("二次写归因失败: %v", err)
	}
	rows2, _ := s.TraceRepo().ListCalibrations(ctx, "20260901", "20260901")
	if len(rows2) != 1 {
		t.Errorf("重跑后归因行数 = %d，期望仍为 1（幂等覆盖）", len(rows2))
	}
}
