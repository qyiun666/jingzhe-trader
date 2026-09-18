package scheduler

import (
	"context"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
)

// TestEmptyReasonRegime 关闸（大盘跌破 MA60）必须被识别成"设计内关闭"而不是普通筛空：
// 用户看到的邮件要说清"今天为什么不买"，并给出若开闸会选到谁的影子清单。
func TestEmptyReasonRegime(t *testing.T) {
	rep := &screener.Report{
		RegimeClosed: true,
		ScoredTotal:  1180,
		Stages: []screener.StageStat{
			{Slug: "elig", Name: "基础资格", In: 5000, Out: 4800},
			{Slug: "regime", Name: "大盘门槛(关闭，下方为影子)", In: 4800, Out: 4800},
		},
		Shadow: []model.Candidate{
			{Rank: 1, TsCode: "600000.SH", Name: "浦发银行", Industry: "银行", Score: 71.3},
		},
	}
	got := emptyReason(rep)
	for _, want := range []string{"MA60", "非故障", "浦发银行"} {
		if !strings.Contains(got, want) {
			t.Errorf("关闸原因应含 %q，实际: %q", want, got)
		}
	}
}

// TestEmptyReasonRegimeFlagIsTheOnlyTrigger 闸门判定只看 RegimeClosed 旗标：
// 关闸时漏斗各级都不再清零，若还按"某级 Out==0"猜闸门，这条就会把设计内关闭误报成故障。
func TestEmptyReasonRegimeFlagIsTheOnlyTrigger(t *testing.T) {
	rep := &screener.Report{ScoredTotal: 0, Stages: []screener.StageStat{
		{Slug: "elig", Name: "基础资格", In: 5000, Out: 0},
	}}
	got := emptyReason(rep)
	if strings.Contains(got, "非故障") {
		t.Errorf("未置 RegimeClosed 时不得宣称非故障，实际: %q", got)
	}
	if !strings.Contains(got, "基础资格") {
		t.Errorf("应点名筛光池子的那一级，实际: %q", got)
	}
}

// TestCandidateExpectTracksRegime 候选产出物的期望随闸门：关闸 0（不出单是结论），
// 开闸 -1（至少 1 只，否则漏斗就是黑的）。关闸仍按 -1 断言会让每个关闸日
// 稳定产出一条假 ARTIFACT_MISSING。
func TestCandidateExpectTracksRegime(t *testing.T) {
	if got := candidateExpect(&screener.Report{RegimeClosed: true}); got != 0 {
		t.Errorf("关闸时候选期望应为 0，实际 %d", got)
	}
	if got := candidateExpect(&screener.Report{}); got != -1 {
		t.Errorf("开闸时候选期望应为 -1（至少 1），实际 %d", got)
	}
	// 期望 0 必须真的不产生缺失项，否则改期望值没有意义。
	rc := observability.NewRunCtx(context.Background(), "evening_pipeline", "20260918")
	rc.Declare("rows", "candidates", candidateExpect(&screener.Report{RegimeClosed: true}))
	rc.Actual("candidates", 0)
	if misses := rc.Assert(); len(misses) != 0 {
		t.Errorf("关闸日 0 候选不应判为产出物缺失，实际 %v", misses)
	}
}

// TestEmptyReasonFunnelStage 非关闸筛空：报出把池子筛到 0 的那一级。
func TestEmptyReasonFunnelStage(t *testing.T) {
	rep := &screener.Report{ScoredTotal: 0, Stages: []screener.StageStat{
		{Slug: "elig", Name: "基础资格", In: 5000, Out: 120},
		{Slug: "liq", Name: "流动性(市值/换手)", In: 120, Out: 0},
	}}
	got := emptyReason(rep)
	if !strings.Contains(got, "流动性(市值/换手)") {
		t.Errorf("应点名筛光候选的那一级，实际: %q", got)
	}
}

// TestTradeConclusionReadsLatestPipelineTrace 计划/日报引用的是"最近一次收盘流水线的结论"：
// 当日 16:30 之前读不到当日行时，应取到上一交易日那行（跨日引用），且带上原因文本。
func TestTradeConclusionReadsLatestPipelineTrace(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/concl.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()

	// 上一交易日（0908）的流水线：关闸降级行
	if err := st.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: "20260908", Subject: model.TraceJob(JobEveningPipeline),
		Outcome: model.TracePartial, Detail: "降级 SCREEN_EMPTY: 大盘在 MA60 下方，当日按规则关闭买入漏斗（非故障）",
		At: "2026-09-08T08:30:00Z",
	}); err != nil {
		t.Fatalf("写轨迹失败: %v", err)
	}

	// 模拟次日早上 09:00：按 date=20260909 取，应回落到 0908 那行。
	d := Deps{Store: st}
	got := tradeConclusion(ctx, d, "20260909")
	if !strings.Contains(got, "20260908") || !strings.Contains(got, "MA60") {
		t.Errorf("结论应引用上一交易日的关闸原因，实际: %q", got)
	}

	// 无任何记录时给一句明确的占位，而不是空串（空串会让邮件少一段却看不出原因）。
	empty := tradeConclusion(ctx, Deps{Store: st}, "20260901")
	if !strings.Contains(empty, "暂无") {
		t.Errorf("无记录时应给占位说明，实际: %q", empty)
	}
}
