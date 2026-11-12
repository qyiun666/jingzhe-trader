package screener

import (
	"context"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// 影子清单空集可区分（派工 20261112 §三 唯一工单项）。
//
// 改动前 SectorShadowBrief 用 len(...)==0 一把返回空串，把三种日子压成同一个数据面：
//
//	① screen.sector_shadow=false             → 无键
//	② 开关 true 且 RegimeClosed=true（互斥）  → 无键
//	③ 开关 true、清单算过但当日 0 淘汰         → 无键  ← 与 ① 同形，就是 11-10 的坑
//
// 现在 ③ 必须写出键，值为一句"板块影子 0 只…"；①② 的"无键"与逐字节同型不许动。
// 载体是 Report.SectorShadow 的 nil（没算过）vs 空非 nil（算过、结果为空），
// 所以每个用例都同时断言"清单形状"与"摘要文案"两层 —— 只断言其中一层抓不到赋值端。

// emptyShadowBriefText 把文案原字写在这里：改措辞必须同时改这里，
// 让"改观测口径的文案"成为一个看得见的动作，而不是顺手重构掉的一句常量。
const emptyShadowBriefText = "板块影子 0 只（sector 级淘汰，未参与打分）：观测开关已开启，当日 sector 级淘汰为空（上游各级先筛空时同样为 0）。"

// TestSectorShadowBriefTriState 摘要层的三态矩阵（纯函数，不碰库）。
func TestSectorShadowBriefTriState(t *testing.T) {
	cases := []struct {
		name   string
		report *Report
		want   string // 期望整句原文；空串即断言"不写键"
	}{
		{"①开关关闭：清单 nil", &Report{}, ""},
		{"②互斥：RegimeClosed + 清单 nil", &Report{RegimeClosed: true}, ""},
		{"②互斥：RegimeClosed + 空清单", &Report{RegimeClosed: true, SectorShadow: []model.Candidate{}}, ""},
		{"②互斥：RegimeClosed + 有清单（输出侧独立保险）", &Report{RegimeClosed: true, SectorShadow: mkShadow(3, 2)}, ""},
		{"③已开启 + 当日 0 淘汰", &Report{SectorShadow: []model.Candidate{}}, emptyShadowBriefText},
	}
	for _, tc := range cases {
		if got := tc.report.SectorShadowBrief(); got != tc.want {
			t.Errorf("%s：期望=%q 实际=%q", tc.name, tc.want, got)
		}
	}
}

// TestSectorShadowBriefEmptyNotConfusedWithDisabled 本工单核心诉求的直接断言：
// 两个 Report 只差 nil / 空非 nil，摘要必须一个空串、一个非空 —— 两者同形就是要修的缺陷。
func TestSectorShadowBriefEmptyNotConfusedWithDisabled(t *testing.T) {
	disabled := (&Report{}).SectorShadowBrief()
	empty := (&Report{SectorShadow: []model.Candidate{}}).SectorShadowBrief()
	if disabled != "" {
		t.Fatalf("关闭态摘要必须为空串（逐字节同型的支点），实际=%q", disabled)
	}
	if empty == "" {
		t.Fatal("开关开启且当日 0 淘汰时摘要不得为空串：与关闭态同形 = 本工单要修的缺陷")
	}
	if !strings.HasPrefix(empty, "板块影子 0 只") {
		t.Errorf("空集句须以「板块影子 0 只」开头（与 N 只句同前缀，一句 grep 捞两种日子），实际=%q", empty)
	}
	if !strings.Contains(empty, "观测开关已开启") {
		t.Errorf("空集句必须自证开关已开启（本句唯一的使命），实际=%q", empty)
	}
	if strings.Contains(empty, "漏斗输入序") || strings.Contains(empty, "列前") {
		t.Errorf("0 只时既无清单也无顺序，不得声称输入序/列前 N: %q", empty)
	}
	if strings.Contains(empty, ".SH") {
		t.Errorf("0 只时不得混进任何标的记录: %q", empty)
	}
	if n := len([]rune(empty)); n > sectorShadowBriefMax {
		t.Errorf("空集句 %d 字符，超过预算 %d", n, sectorShadowBriefMax)
	}
}

// TestSectorShadowBriefFormatLockNonEmpty 回归锁：有清单那一态的文案必须与 11-12 首批实盘
// 落地原文同型（本工单不许顺手动它）。
func TestSectorShadowBriefFormatLockNonEmpty(t *testing.T) {
	one := (&Report{SectorShadow: []model.Candidate{{
		TsCode: "600000.SH", Name: "浦发银行", Industry: "银行",
		Close: model.Fen(1179), SectorShadowFiltered: true,
	}}}).SectorShadowBrief()
	want := "板块影子 1 只（sector 级淘汰，未参与打分，漏斗输入序）：600000.SH 浦发银行/银行 11.79元。"
	if one != want {
		t.Errorf("单只摘要漂移\n期望=%q\n实际=%q", want, one)
	}

	head := (&Report{SectorShadow: mkShadow(sectorShadowTop+2, 2)}).SectorShadowBrief()
	wantHead := "板块影子 12 只（sector 级淘汰，未参与打分，漏斗输入序，列前 10）："
	if !strings.HasPrefix(head, wantHead) {
		t.Errorf("超出 TopN 时头部漂移\n期望前缀=%q\n实际=%q", wantHead, head)
	}
}

// TestSectorShadowFunnelTriStateEndToEnd 走完整条 Run()：同一份种子数据，开关 × 板块门槛
// × 闸门四格，断言只有 ③（开启 + 未关闸 + 0 淘汰）那一格写出键，其余三格保持空串。
func TestSectorShadowFunnelTriStateEndToEnd(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name            string
		shadow          bool
		topK            int
		marketOK        bool
		wantNilList     bool
		wantBrief       string // 整句；与 wantBriefPrefix 互斥
		wantBriefPrefix string // 只锁开头（有清单那格）
		wantDrops       int    // sector 级"非强势板块"淘汰数：开关不得改动
		wantCands       int
	}{
		// ① 开关关闭：门槛一只没挡，也不得留任何影子痕迹。
		{name: "关闭+0淘汰", shadow: false, topK: 1, marketOK: true, wantNilList: true,
			wantDrops: 0, wantCands: shadowCodes},
		// ③ 开关开启 + 当日 0 淘汰：非 nil 空清单 + 「0 只」句 —— 本次新增的形态。
		{name: "开启+0淘汰", shadow: true, topK: 1, marketOK: true, wantNilList: false,
			wantBrief: emptyShadowBriefText, wantDrops: 0, wantCands: shadowCodes},
		// 对照：开关开启 + 有淘汰，行为与 11-12 实测一致（总数句，不进 0 只分支）。
		{name: "开启+全淘汰", shadow: true, topK: 0, marketOK: true, wantNilList: false,
			wantBriefPrefix: "板块影子 12 只（", wantDrops: shadowCodes, wantCands: 0},
		// ② 互斥：开关开启但当日关闸，清单仍 nil、摘要仍空（维持现状）。
		{name: "开启+关闸", shadow: true, topK: 1, marketOK: false, wantNilList: true,
			wantDrops: 0, wantCands: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := seedShadow(t)
			cfg := shadowCfg()
			cfg.SectorShadow = tc.shadow
			cfg.SectorTopK = tc.topK
			rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(tc.marketOK))
			if err != nil {
				t.Fatalf("跑漏斗失败: %v", err)
			}
			if !tc.marketOK && !rep.RegimeClosed {
				t.Fatal("MarketOK=false 时 RegimeClosed 应为 true")
			}
			// 清单形状是三态的载体：nil / 空非 nil / 非空 必须各就各位。
			if got := rep.SectorShadow == nil; got != tc.wantNilList {
				t.Errorf("清单 nil 旗标=%v，期望=%v（实际 %d 条）", got, tc.wantNilList, len(rep.SectorShadow))
			}
			if !tc.wantNilList && tc.wantBrief == "" && tc.wantBriefPrefix == "" {
				if len(rep.SectorShadow) == 0 {
					t.Fatal("该格期望非 nil 空清单，实际是空且 nil —— 赋值端没跑")
				}
			}
			got := rep.SectorShadowBrief()
			switch {
			case tc.wantBrief != "":
				if got != tc.wantBrief {
					t.Errorf("摘要失真\n期望=%q\n实际=%q", tc.wantBrief, got)
				}
			case tc.wantBriefPrefix != "":
				if !strings.HasPrefix(got, tc.wantBriefPrefix) {
					t.Errorf("有清单那格须以总数句开头 %q，实际=%q", tc.wantBriefPrefix, got)
				}
			default:
				if got != "" {
					t.Errorf("该格不得写出摘要（载荷会多出一个键），实际=%q", got)
				}
			}
			if n := sectorDropCount(rep); n != tc.wantDrops {
				t.Errorf("sector drops=%d，期望 %d（开关与观测不得吃掉淘汰计数）", n, tc.wantDrops)
			}
			if n := len(rep.Candidates); n != tc.wantCands {
				t.Errorf("候选数=%d，期望 %d（观测不得改变决策链输入）", n, tc.wantCands)
			}
		})
	}
}

// TestSectorShadowEmptyListChangesNothingButTheBrief 0 淘汰日的开关只许影响清单与摘要：
// 各级进出、漏斗文案、Notes、候选口径全部同型 —— 与全淘汰日的
// TestSectorShadowEnabledChangesNothingButTheShadowList 配成一对，覆盖开关影响面的两端。
func TestSectorShadowEmptyListChangesNothingButTheBrief(t *testing.T) {
	ctx := context.Background()

	run := func(on bool) *Report {
		t.Helper()
		st := seedShadow(t)
		cfg := shadowCfg()
		cfg.SectorShadow = on
		cfg.SectorTopK = 1 // 12 只同行业且行业进 hot ⇒ sector 级 0 淘汰
		rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
		if err != nil {
			t.Fatalf("跑漏斗失败（sector_shadow=%v）: %v", on, err)
		}
		return rep
	}

	off, on := run(false), run(true)
	if off.SectorShadow != nil {
		t.Errorf("关闭态 SectorShadow 必须保持 nil（不遍历、不分配），实际 %d 条", len(off.SectorShadow))
	}
	if on.SectorShadow == nil {
		t.Fatal("开启态 SectorShadow 必须是**非 nil** 空切片，否则与关闭态不可分")
	}
	if len(on.SectorShadow) != 0 {
		t.Fatalf("本用例的数据应当 0 淘汰，实际 %d 条", len(on.SectorShadow))
	}
	if diff := stageDiff(off.Stages, on.Stages); diff != "" {
		t.Errorf("开关改动了漏斗各级:\n%s", diff)
	}
	if got, want := funnelSummary(on.Stages), funnelSummary(off.Stages); got != want {
		t.Errorf("漏斗文案漂移:\n开=%s\n关=%s", got, want)
	}
	if strings.Join(on.Notes, "|") != strings.Join(off.Notes, "|") {
		t.Errorf("Notes 漂移:\n开=%v\n关=%v", on.Notes, off.Notes)
	}
	if len(off.Candidates) != len(on.Candidates) || off.Empty != on.Empty ||
		off.ScoredTotal != on.ScoredTotal || len(off.Watch) != len(on.Watch) {
		t.Errorf("候选口径漂移：开=%d/%d empty=%v，关=%d/%d empty=%v",
			len(on.Candidates), on.ScoredTotal, on.Empty,
			len(off.Candidates), off.ScoredTotal, off.Empty)
	}
	// 关闭态仍是空串（逐字节同型的支点没被削弱），开启态才有那句 0 只。
	if got := off.SectorShadowBrief(); got != "" {
		t.Errorf("关闭态摘要必须为空串，实际=%q", got)
	}
	if got := on.SectorShadowBrief(); got != emptyShadowBriefText {
		t.Errorf("开启态 0 淘汰摘要=%q，期望=%q", got, emptyShadowBriefText)
	}
}

// sectorDropCount 取漏斗 sector 级的"非强势板块"淘汰数（正常应与影子清单条数同数）。
func sectorDropCount(rep *Report) int {
	for _, stage := range rep.Stages {
		if stage.Slug == "sector" {
			return stage.Drops[reasonSectorOut]
		}
	}
	return -1
}
