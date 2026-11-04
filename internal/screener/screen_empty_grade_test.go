package screener

import (
	"context"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// TestLotWindowDropsSumsStages 整手窗口淘汰数按级汇总（B1 + 组长 20261104 批复的判据）。
func TestLotWindowDropsSumsStages(t *testing.T) {
	r := &Report{Stages: []StageStat{
		{Slug: "sector", Drops: map[string]int{reasonSectorOut: 5126}},
		{Slug: "budget", Name: "可用资金(一手≤预算)", Drops: map[string]int{
			ReasonLotWindow: 13, reasonUnaffordable: 4}},
		{Slug: "liq", Drops: map[string]int{reasonIlliquid: 30}},
	}}
	if got := r.LotWindowDrops(); got != 13 {
		t.Errorf("整手窗口淘汰数应为 13，实际 %d", got)
	}
	empty := &Report{}
	if got := empty.LotWindowDrops(); got != 0 {
		t.Errorf("无留痕时应为 0，实际 %d", got)
	}
	// 只有"买不起"（上界）而没有"整手落不进窗口"（下界）时不得算作设计内：
	// 高价股被单票上限拦掉是另一件事，判据不能顺手合并。
	upperOnly := &Report{Stages: []StageStat{{Slug: "budget", Drops: map[string]int{reasonUnaffordable: 20}}}}
	if got := upperOnly.LotWindowDrops(); got != 0 {
		t.Errorf("只判上界的淘汰不该计入整手窗口，实际 %d", got)
	}
}

// TestScreenEmptyGradedByReason 0 候选的三级判据：关闸 partial、整手窗口 partial、
// 其余筛光 fail。这里守的是两件事：
//  1. 设计内的筛光不许喊"请人工介入"（B1 上线后这是弱势小账户的合法状态）；
//  2. 判据必须**按淘汰原因**成立，不许因为"看到 0 候选"就一律降级——
//     那样数据故障会被新出口吞掉，从"天天误报"翻成"永远不报"。
func TestScreenEmptyGradedByReason(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/grade.db")
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st, shadowCfg(), ModeReversal)
	date := "20260720"

	cases := []struct {
		name      string
		rep       *Report
		wantOK    string
		notWantOK string
	}{
		{
			name: "整手窗口筛光=设计内partial",
			rep: &Report{ScoredTotal: 54, Stages: []StageStat{
				{Slug: "budget", Name: "可用资金(一手≤预算)", In: 425, Out: 0,
					Drops: map[string]int{ReasonLotWindow: 13, reasonUnaffordable: 6}}},
			},
			wantOK: "设计内（B1 整手窗口淘汰）", notWantOK: "请人工介入",
		},
		{
			name: "估值筛光=故障fail",
			rep: &Report{ScoredTotal: 54, Stages: []StageStat{
				{Slug: "val", Name: "估值(价格/PE/PB)", In: 84, Out: 0,
					Drops: map[string]int{reasonBadPE: 60, reasonBadPB: 24}}},
			},
			wantOK: "请人工介入", notWantOK: "设计内",
		},
	}
	for _, c := range cases {
		if err := s.raiseEmptyAlert(ctx, date, c.rep, "漏斗摘要"); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		tr, ok, err := st.TraceRepo().LatestBySubject(ctx, model.TraceAlert(AlertCodeScreenEmpty), date)
		if err != nil || !ok {
			t.Fatalf("%s: 应落 SCREEN_EMPTY，实际存在=%t err=%v", c.name, ok, err)
		}
		want := model.TraceFail
		if strings.Contains(c.wantOK, "设计内") {
			want = model.TracePartial
		}
		if tr.Outcome != want {
			t.Errorf("%s: 定级应为 %q，实际 %q（正文 %s）", c.name, want, tr.Outcome, tr.Detail)
		}
		if !strings.Contains(tr.Detail, c.wantOK) {
			t.Errorf("%s: 正文应含 %q，实际 %s", c.name, c.wantOK, tr.Detail)
		}
		if strings.Contains(tr.Detail, c.notWantOK) {
			t.Errorf("%s: 正文不该含 %q，实际 %s", c.name, c.notWantOK, tr.Detail)
		}
	}

	// 关闸优先级最高：关闸日同时有整手窗口淘汰时，仍走关闸文案 + 影子清单，
	// 不能因为新分支把"若开闸会选到谁"这段证据挤掉。
	closed := &Report{RegimeClosed: true, RegimeMA: 60, ScoredTotal: 40, Shadow: []model.Candidate{
		{TsCode: "600001.SH", Name: "甲", Rank: 1}},
		Stages: []StageStat{{Slug: "budget", Drops: map[string]int{ReasonLotWindow: 7}}}}
	if err := s.raiseEmptyAlert(ctx, "20260721", closed, "漏斗摘要"); err != nil {
		t.Fatalf("关闸分级: %v", err)
	}
	tr, ok, err := st.TraceRepo().LatestBySubject(ctx, model.TraceAlert(AlertCodeScreenEmpty), "20260721")
	if err != nil || !ok {
		t.Fatalf("关闸应落轨迹，实际存在=%t err=%v", ok, err)
	}
	if tr.Outcome != model.TracePartial || !strings.Contains(tr.Detail, "若开闸") {
		t.Errorf("关闸日必须是 partial 且带影子清单，实际 %q / %s", tr.Outcome, tr.Detail)
	}
	if strings.Contains(tr.Detail, "B1 整手窗口淘汰") {
		t.Errorf("关闸文案不该混进整手窗口判据（两种 0 候选的读者动作不同），实际: %s", tr.Detail)
	}
}
