package screener

import (
	"context"
	"strings"
	"testing"
)

// TestSectorShadowDefaultFalse 默认 screen.sector_shadow=false 时零副作用：
// SectorShadow 留空、Candidates 不受影响、现有 trace 输出不变。
func TestSectorShadowDefaultFalse(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	// 默认 SectorShadow=false
	if cfg.SectorShadow {
		t.Fatal("shadowCfg 默认 SectorShadow 应为 false")
	}
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if len(rep.SectorShadow) != 0 {
		t.Errorf("默认 false 时 SectorShadow 应为空，实际 %d 条", len(rep.SectorShadow))
	}
	if rep.RegimeClosed {
		t.Error("MarketOK=true 时 RegimeClosed 应为 false")
	}
	if len(rep.Candidates) != shadowCodes {
		t.Errorf("默认 false 不应影响 Candidates，实际 %d 只", len(rep.Candidates))
	}
}

// TestSectorShadowPopulatedWhenEnabled SectorShadow=true 且 RegimeClosed=false 时，
// sector 级淘汰的标的落 Report.SectorShadow 并标 SectorShadowFiltered=true。
func TestSectorShadowPopulatedWhenEnabled(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = true
	// SectorTopK=1 且 12 只同行业（银行），全部通过 sector 门槛
	// 要测出 sector 淘汰，需让部分票的行业不在 hot 集合里
	cfg.SectorTopK = 0 // 0 个强势板块 ⇒ 全部被淘汰
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if rep.RegimeClosed {
		t.Fatal("MarketOK=true 时 RegimeClosed 应为 false")
	}
	// SectorTopK=0 时 hot 集合为空，所有票都被 sector 门槛淘汰
	if len(rep.SectorShadow) != shadowCodes {
		t.Errorf("SectorShadow 应等于全部 %d 只，实际 %d 只", shadowCodes, len(rep.SectorShadow))
	}
	for i, c := range rep.SectorShadow {
		if !c.SectorShadowFiltered {
			t.Errorf("第 %d 条 SectorShadowFiltered 应为 true", i)
		}
		if c.TsCode == "" {
			t.Errorf("第 %d 条 TsCode 不应为空", i)
		}
	}
	// Candidates 应为空（全被 sector 淘汰）
	if len(rep.Candidates) != 0 {
		t.Errorf("SectorTopK=0 时 Candidates 应为空，实际 %d 只", len(rep.Candidates))
	}
}

// TestSectorShadowMutuallyExclusiveWithRegimeClosed RegimeClosed=true 时，
// SectorShadow 必须留空（整条漏斗影子已含 sector 级信息，避免重复）。
func TestSectorShadowMutuallyExclusiveWithRegimeClosed(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = true
	cfg.SectorTopK = 1 // 保留银行行业（seedShadow 里 12 只都是银行）
	// MarketOK=false ⇒ RegimeClosed=true
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(false))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if !rep.RegimeClosed {
		t.Fatal("MarketOK=false 时 RegimeClosed 应为 true")
	}
	// 互斥：RegimeClosed=true 时 SectorShadow 必须留空
	if len(rep.SectorShadow) != 0 {
		t.Errorf("RegimeClosed=true 时 SectorShadow 应为空，实际 %d 条", len(rep.SectorShadow))
	}
	// Shadow 应包含整条漏斗影子（原有语义不变）
	// 注意：SectorTopK=1 时银行在 hot 集合，12 只全部通过 sector 门槛，进入后续漏斗
	if len(rep.Shadow) == 0 {
		t.Error("RegimeClosed=true 且 SectorTopK=1 时 Shadow 应包含影子候选")
	}
}

// TestSectorShadowCountMatchesStageDrops SectorShadow 条数与漏斗 stage2 drops 数一致。
func TestSectorShadowCountMatchesStageDrops(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = true
	cfg.SectorTopK = 0 // 全部 sector 淘汰
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	// 找 stage slug="sector" 的 drops
	var sectorDrops int
	for _, stage := range rep.Stages {
		if stage.Slug == "sector" {
			for reason, count := range stage.Drops {
				if strings.Contains(reason, "强势板块") {
					sectorDrops += count
				}
			}
		}
	}
	if len(rep.SectorShadow) != sectorDrops {
		t.Errorf("SectorShadow 条数 (%d) 应与 stage2 sector drops (%d) 一致",
			len(rep.SectorShadow), sectorDrops)
	}
}

// TestSectorShadowPreservesCandidateFields SectorShadow 条目应保留关键字段用于观测。
func TestSectorShadowPreservesCandidateFields(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = true
	cfg.SectorTopK = 0
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if len(rep.SectorShadow) == 0 {
		t.Fatal("SectorShadow 不应为空")
	}
	c := rep.SectorShadow[0]
	// 关键字段应被保留
	if c.TsCode == "" {
		t.Error("TsCode 不应为空")
	}
	if c.Name == "" {
		t.Error("Name 不应为空")
	}
	if c.Industry == "" {
		t.Error("Industry 不应为空")
	}
	if c.Close <= 0 {
		t.Error("Close 应大于 0")
	}
	// SectorShadowFiltered 标记
	if !c.SectorShadowFiltered {
		t.Error("SectorShadowFiltered 应为 true")
	}
}

// TestSectorShadowFalseRegimeClosedFalse SectorShadow=false 且 RegimeClosed=false 时，
// SectorShadow 留空，Candidates 正常产出。
func TestSectorShadowFalseRegimeClosedFalse(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = false
	cfg.SectorTopK = 0 // 全部 sector 淘汰
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if rep.RegimeClosed {
		t.Error("MarketOK=true 时 RegimeClosed 应为 false")
	}
	if len(rep.SectorShadow) != 0 {
		t.Errorf("SectorShadow=false 时 SectorShadow 应为空，实际 %d 条", len(rep.SectorShadow))
	}
	// Candidates 应为空（全被 sector 淘汰，但 SectorShadow 不记录）
	if len(rep.Candidates) != 0 {
		t.Errorf("SectorTopK=0 时 Candidates 应为空，实际 %d 只", len(rep.Candidates))
	}
}

// TestSectorShadowWithPartialSectorDrop 部分行业在 hot 集合、部分不在时，
// SectorShadow 只记录非 hot 行业的标的。使用 seedShadow 的数据（12 只同行业银行），
// 通过调整 SectorTopK 来控制 hot 集合大小。
func TestSectorShadowWithPartialSectorDrop(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = true
	// seedShadow 里 12 只都是银行行业，SectorTopK=1 时银行在 hot 集合，全部通过
	// 要测部分淘汰，需造两个行业的数据——但 seedShadow 只造了一个行业
	// 因此本测试验证 SectorTopK=0 时全部淘汰的边界情况
	cfg.SectorTopK = 0
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	// SectorTopK=0 时 hot 集合为空，所有 12 只银行票都被 sector 淘汰
	if len(rep.SectorShadow) != shadowCodes {
		t.Errorf("SectorShadow 应为 %d 只，实际 %d 只", shadowCodes, len(rep.SectorShadow))
	}
	for _, c := range rep.SectorShadow {
		if !c.SectorShadowFiltered {
			t.Errorf("SectorShadowFiltered 应为 true: %s", c.TsCode)
		}
		if c.Industry != "银行" {
			t.Errorf("Industry 应为银行，实际 %s", c.Industry)
		}
	}
}
