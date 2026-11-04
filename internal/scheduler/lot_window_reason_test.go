package scheduler

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/screener"
)

// TestEmptyReasonDisclosesLotWindow emptyReason 必须与 screener.raiseEmptyAlert 的分级
// 同口径（20261104 组长批复②）：trace 说"设计内"而邮件说"筛光候选"时，读的人仍会当成
// 故障去回滚——一句文案的漂移就废掉整个判据。
func TestEmptyReasonDisclosesLotWindow(t *testing.T) {
	byLotWindow := &screener.Report{ScoredTotal: 54, Stages: []screener.StageStat{
		{Slug: "sector", Name: "板块强弱TopK", In: 5351, Out: 425},
		{Slug: "budget", Name: "可用资金(一手≤预算)", In: 425, Out: 0,
			Drops: map[string]int{screener.ReasonLotWindow: 13}},
	}}
	line := emptyReason(byLotWindow)
	for _, want := range []string{"设计内", "整手", "非数据故障", "可用资金"} {
		if !strings.Contains(line, want) {
			t.Errorf("整手窗口筛光的降级文案应含 %q，实际: %q", want, line)
		}
	}

	byValuation := &screener.Report{ScoredTotal: 84, Stages: []screener.StageStat{
		{Slug: "val", Name: "估值(价格/PE/PB)", In: 84, Out: 0,
			Drops: map[string]int{"亏损或PE超标": 60}},
	}}
	line2 := emptyReason(byValuation)
	if !strings.Contains(line2, "筛光候选") {
		t.Errorf("真实筛光仍要说\"筛光候选\"，不许被设计内出口顺带洗白，实际: %q", line2)
	}
	if strings.Contains(line2, "设计内") {
		t.Errorf("非整手窗口的筛光不该出现\"设计内\"，实际: %q", line2)
	}

	// 只判上界（一手超过单票上限）不是设计内：那是仓位口径的正常拦截，与整手空洞不同事。
	upperOnly := &screener.Report{ScoredTotal: 425, Stages: []screener.StageStat{
		{Slug: "budget", Name: "可用资金(一手≤预算)", In: 425, Out: 0,
			Drops: map[string]int{"一手超单笔预算": 425}},
	}}
	if got := emptyReason(upperOnly); strings.Contains(got, "设计内") {
		t.Errorf("上界淘汰不该读成设计内，实际: %q", got)
	}
}
