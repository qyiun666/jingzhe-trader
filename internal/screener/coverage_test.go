package screener

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestValuationCoverageReported 正常同步日：覆盖率必须写进 Report 与 Notes。
// 覆盖率是读侧事实（val_date == 选股日 的只数），不是同步接口的返回值。
func TestValuationCoverageReported(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	rep, err := New(st, shadowCfg(), ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if rep.ValuationTotal != shadowCodes || rep.ValuationCovered != shadowCodes {
		t.Errorf("覆盖=%d/%d，期望 %d/%d", rep.ValuationCovered, rep.ValuationTotal, shadowCodes, shadowCodes)
	}
	if rep.ValuationCoverage != 1 {
		t.Errorf("覆盖率=%.3f，期望 1", rep.ValuationCoverage)
	}
	if rep.ValuationSparse {
		t.Error("全覆盖不应置 ValuationSparse")
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "估值截面覆盖 12/12") {
		t.Errorf("Notes 缺少覆盖率: %v", rep.Notes)
	}
}

// TestValuationMissingHasOwnStage 7/12 只没有当日估值：它们必须在独立的
// "估值数据完整性"级被剔掉并记 reasonNoValuation，不能混进资金/流动性/估值筛选；
// 覆盖率降到 41.7%，触发 Sparse 让上层落降级。
func TestValuationMissingHasOwnStage(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	// 把 7 只的估值截面改成前一交易日：val_date != 选股日，即"今天没有估值"。
	vals := make([]model.Valuation, 0, shadowCodes-5)
	for i := 5; i < shadowCodes; i++ {
		vals = append(vals, model.Valuation{
			TsCode: fmt.Sprintf("60%04d.SH", i), TurnoverRate: 1.5,
			PETtm: 20, PB: 2, CircMvW: 600000,
		})
	}
	if err := st.MarketRepo().SaveValuation(ctx, "20260719", vals); err != nil {
		t.Fatalf("改写估值截面失败: %v", err)
	}

	rep, err := New(st, shadowCfg(), ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if rep.ValuationCovered != 5 || rep.ValuationTotal != shadowCodes {
		t.Errorf("覆盖=%d/%d，期望 5/%d", rep.ValuationCovered, rep.ValuationTotal, shadowCodes)
	}
	if !rep.ValuationSparse {
		t.Error("覆盖率 41.7% 应置 ValuationSparse")
	}
	if len(rep.Candidates) != 5 {
		t.Errorf("有当日估值的 5 只应成为候选，实际 %d 只", len(rep.Candidates))
	}

	var valdata *StageStat
	for i := range rep.Stages {
		if rep.Stages[i].Slug == "valdata" {
			valdata = &rep.Stages[i]
		}
	}
	if valdata == nil {
		t.Fatalf("缺少 valdata 级，实际各级: %s", stageSlugs(rep))
	}
	if valdata.In != shadowCodes || valdata.Out != 5 {
		t.Errorf("估值完整性级应为 %d→5，实际 %d→%d", shadowCodes, valdata.In, valdata.Out)
	}
	if got := valdata.Drops[reasonNoValuation]; got != shadowCodes-5 {
		t.Errorf("缺失原因计数=%d，期望 %d（drops=%v）", got, shadowCodes-5, valdata.Drops)
	}
	// 后续各级不该再背"估值缺失"的锅：原因归属只有一处。
	for _, st := range rep.Stages {
		if st.Slug == "valdata" {
			continue
		}
		if st.Drops[reasonNoValuation] > 0 {
			t.Errorf("%s 级混入了估值缺失淘汰: %v", st.Slug, st.Drops)
		}
	}
}
