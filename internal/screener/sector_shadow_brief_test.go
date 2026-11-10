package screener

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"jingzhe-trader/internal/model"
)

// B2 输出侧接线（SectorShadowBrief → screen:topn 快照）的回归。
//
// 派工 20261110 第 7 条要求的四条都在这个文件里：
//
//	① 内容含总数与前 N 只、超长被截断       → TestSectorShadowBriefContentAndTruncation
//	② 开关 false 零开销（不产出元素/摘要）   → TestSectorShadowBriefZeroCostWhenDisabled
//	③ RegimeClosed 互斥（清单留空、摘要不出现）→ TestSectorShadowBriefMutuallyExclusiveWithRegimeClosed
//	④ 附加而非替换（sector 级计数与既有文案不变）→ TestSectorShadowEnabledChangesNothingButTheShadowList
//
// 载荷侧（字节级"只追加"）在 scheduler 包：TestWriteScreenSnapshotSectorShadowField。

// mkShadow 造 n 只板块影子元素（只填摘要用得到的四个字段，其余留零值）。
func mkShadow(n int, industryLen int) []model.Candidate {
	out := make([]model.Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, model.Candidate{
			TsCode:               fmt.Sprintf("60%04d.SH", i),
			Name:                 fmt.Sprintf("测试%d", i),
			Industry:             strings.Repeat("行", industryLen),
			Close:                model.Fen(1000 + 10*i),
			SectorShadowFiltered: true,
		})
	}
	return out
}

// TestSectorShadowBriefContentAndTruncation 摘要必须报出精确总数、最多列 sectorShadowTop 只，
// 且整段长度永远不超过 sectorShadowBriefMax —— 这一句最终进 run_trace.detail，
// 会被 get_logs 与日报整段读出来，撑爆它就是把观测手段变成新的故障源。
func TestSectorShadowBriefContentAndTruncation(t *testing.T) {
	// 不足 TopN：全部列出，不得出现"列前"字样（列全了还说列前 N 是失真）。
	brief := (&Report{SectorShadow: mkShadow(3, 2)}).SectorShadowBrief()
	if !strings.Contains(brief, "板块影子 3 只") {
		t.Errorf("摘要未报总数，实际=%q", brief)
	}
	if strings.Contains(brief, "列前") {
		t.Errorf("3 只全列时不应出现「列前」，实际=%q", brief)
	}
	for i := 0; i < 3; i++ {
		code := fmt.Sprintf("60%04d.SH", i)
		if !strings.Contains(brief, code) {
			t.Errorf("摘要缺少 %s，实际=%q", code, brief)
		}
	}
	if !strings.Contains(brief, "测试0") || !strings.Contains(brief, "10.00元") {
		t.Errorf("摘要应含名称与收盘价，实际=%q", brief)
	}

	// 超过 TopN：列前 sectorShadowTop 只，总数仍是全量，第 11 只不得出现。
	r := &Report{SectorShadow: mkShadow(12, 2)}
	brief = r.SectorShadowBrief()
	if !strings.Contains(brief, "板块影子 12 只") {
		t.Errorf("总数应为全量 12，实际=%q", brief)
	}
	if !strings.Contains(brief, fmt.Sprintf("列前 %d", sectorShadowTop)) {
		t.Errorf("应标明只列前 %d，实际=%q", sectorShadowTop, brief)
	}
	if got := strings.Count(brief, ".SH"); got != sectorShadowTop {
		t.Errorf("实际列出 %d 只，期望 %d 只: %s", got, sectorShadowTop, brief)
	}
	if strings.Contains(brief, "600010.SH") || strings.Contains(brief, "600011.SH") {
		t.Errorf("超出 TopN 的标的不得混进摘要: %s", brief)
	}
	if utf8.RuneCountInString(brief) > sectorShadowBriefMax {
		t.Errorf("摘要 %d 字符，超过上限 %d", utf8.RuneCountInString(brief), sectorShadowBriefMax)
	}

	// 超长：单条记录足够长时，必须从尾部**整只**丢弃到限长以内，并把实际列出只数如实写进"列前 N"。
	r = &Report{SectorShadow: mkShadow(sectorShadowTop, 100)}
	brief = r.SectorShadowBrief()
	if n := utf8.RuneCountInString(brief); n > sectorShadowBriefMax {
		t.Errorf("超长未截断：%d 字符 > 上限 %d", n, sectorShadowBriefMax)
	}
	listed := strings.Count(brief, ".SH")
	if listed >= sectorShadowTop {
		t.Errorf("超长时列出只数应少于 %d，实际 %d", sectorShadowTop, listed)
	}
	if !strings.Contains(brief, fmt.Sprintf("列前 %d", listed)) {
		t.Errorf("摘要未如实标出实际列出只数 %d: %q", listed, brief)
	}
	if !strings.Contains(brief, "600000.SH") {
		t.Errorf("截断只丢尾部，首只必须还在: %q", brief)
	}
	if strings.Contains(brief, strings.Repeat("行", 100)+fmt.Sprintf(" %.2f元", model.Fen(1000+10*(sectorShadowTop-1)).Float())) {
		t.Errorf("最后一只应已被丢弃: %q", brief)
	}

	// 病态输入：连一只都放不下时退回"只报总数"，长度依然受限且不输出半条记录。
	r = &Report{SectorShadow: mkShadow(1, sectorShadowBriefMax*2)}
	brief = r.SectorShadowBrief()
	if n := utf8.RuneCountInString(brief); n > sectorShadowBriefMax {
		t.Errorf("只报总数的兜底仍超长：%d 字符", n)
	}
	if !strings.Contains(brief, "板块影子 1 只") {
		t.Errorf("兜底必须报出总数，实际=%q", brief)
	}
	if strings.Contains(brief, ".SH") {
		t.Errorf("兜底不得输出半条记录，实际=%q", brief)
	}

	// 总数为 0 返回空串（调用方据此不写任何字段）。
	if got := (&Report{}).SectorShadowBrief(); got != "" {
		t.Errorf("空清单应返回空串，实际=%q", got)
	}
}

// TestSectorShadowBriefZeroCostWhenDisabled 开关关闭时零开销：漏斗不产出任何影子元素
// （SectorShadow 保持 nil 就是"那段遍历一次都没跑"的可观测证据），摘要也就不存在。
func TestSectorShadowBriefZeroCostWhenDisabled(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = false
	cfg.SectorTopK = 0 // 让 sector 级把 12 只全淘汰：这是影子清单最"想"扩张的配置
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if rep.SectorShadow != nil {
		t.Errorf("开关关闭时 SectorShadow 必须保持 nil（不得遍历/装载），实际 %d 条", len(rep.SectorShadow))
	}
	if got := rep.SectorShadowBrief(); got != "" {
		t.Errorf("开关关闭时不得生成摘要，实际=%q", got)
	}
	// 任何流向决策链的对象都不许带影子标记。
	for _, c := range rep.Candidates {
		if c.SectorShadowFiltered {
			t.Errorf("候选 %s 带 SectorShadowFiltered 标记", c.TsCode)
		}
	}
	for _, c := range rep.Watch {
		if c.SectorShadowFiltered {
			t.Errorf("保留带 %s 带 SectorShadowFiltered 标记", c.TsCode)
		}
	}
}

// TestSectorShadowBriefMutuallyExclusiveWithRegimeClosed 关闸日影子清单留空、摘要不得出现：
// 那天已有 ShadowBrief 覆盖整条漏斗的影子，两句叠进同一条 detail 就成了两份重叠口径。
func TestSectorShadowBriefMutuallyExclusiveWithRegimeClosed(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	cfg := shadowCfg()
	cfg.SectorShadow = true
	cfg.SectorTopK = 0 // sector 级会把 12 只全淘汰 —— 若互斥失效，这里就会冒出清单
	rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(false))
	if err != nil {
		t.Fatalf("跑漏斗失败: %v", err)
	}
	if !rep.RegimeClosed {
		t.Fatal("MarketOK=false 时 RegimeClosed 应为 true")
	}
	if len(rep.SectorShadow) != 0 {
		t.Errorf("关闸日 SectorShadow 应留空，实际 %d 条", len(rep.SectorShadow))
	}
	if got := rep.SectorShadowBrief(); got != "" {
		t.Errorf("关闸日不得出现板块影子摘要，实际=%q", got)
	}
	if rep.ShadowBrief() == "" {
		t.Error("关闸日 ShadowBrief 必须仍在（两句互斥，不是同缺）")
	}

	// 输出侧的独立保险：即便将来有人改动产出条件、让关闸日也填了清单，摘要这一句也不得出现。
	r := &Report{RegimeClosed: true, SectorShadow: mkShadow(3, 2)}
	if got := r.SectorShadowBrief(); got != "" {
		t.Errorf("RegimeClosed=true 时 SectorShadowBrief 必须为空串，实际=%q", got)
	}
}

// TestSectorShadowEnabledChangesNothingButTheShadowList 「附加」而非「替换」的总锁：
// 开启开关后，sector 级 drops 计数、各级漏斗进出、Notes、候选数、以及已落库的
// alert:SCREEN_EMPTY detail 文案必须与关闭时逐字相同 —— 唯一允许的差异是 SectorShadow 本身。
// （a8ae15f 的 keys.go/filter.go 注释把它写成"不落 reasonSectorOut 的替换"，与代码不符，
// 本用例连同注释一起把真实语义钉死。）
func TestSectorShadowEnabledChangesNothingButTheShadowList(t *testing.T) {
	ctx := context.Background()

	run := func(on bool) (*Report, *model.RunTrace) {
		t.Helper()
		st := seedShadow(t)
		cfg := shadowCfg()
		cfg.SectorShadow = on
		cfg.SectorTopK = 0
		rep, err := New(st, cfg, ModeReversal).Run(ctx, shadowDate, shadowBudget(true))
		if err != nil {
			t.Fatalf("跑漏斗失败（sector_shadow=%v）: %v", on, err)
		}
		rows, err := st.TraceRepo().List(ctx, shadowDate)
		if err != nil {
			t.Fatalf("读轨迹失败: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("期望恰好一条轨迹（SCREEN_EMPTY 告警），实际 %d 条", len(rows))
		}
		return rep, &rows[0]
	}

	off, offTrace := run(false)
	on, onTrace := run(true)

	// ① sector 级淘汰计数照旧（"非强势板块" 12 只，一级都不能少）。
	var sectorOff, sectorOn *StageStat
	for i := range off.Stages {
		if off.Stages[i].Slug == "sector" {
			sectorOff = &off.Stages[i]
		}
	}
	for i := range on.Stages {
		if on.Stages[i].Slug == "sector" {
			sectorOn = &on.Stages[i]
		}
	}
	if sectorOff == nil || sectorOn == nil {
		t.Fatal("两次漏斗都应有 slug=sector 的一级")
	}
	if got := sectorOff.Drops[reasonSectorOut]; got != shadowCodes {
		t.Errorf("关闭时 sector drops=%d，期望 %d", got, shadowCodes)
	}
	if got := sectorOn.Drops[reasonSectorOut]; got != shadowCodes {
		t.Errorf("开启时 sector drops=%d，期望 %d（开关不得吃掉淘汰计数）", got, shadowCodes)
	}

	// ② 各级进出与摘要文本、Notes、候选数、Empty 旗标全部同型。
	if diff := stageDiff(off.Stages, on.Stages); diff != "" {
		t.Errorf("开启开关改动了漏斗各级:\n%s", diff)
	}
	if got, want := funnelSummary(on.Stages), funnelSummary(off.Stages); got != want {
		t.Errorf("漏斗摘要文案漂移:\n开=%s\n关=%s", got, want)
	}
	if strings.Join(on.Notes, "|") != strings.Join(off.Notes, "|") {
		t.Errorf("Notes 漂移:\n开=%v\n关=%v", on.Notes, off.Notes)
	}
	if off.Empty != on.Empty || off.ScoredTotal != on.ScoredTotal ||
		len(off.Candidates) != len(on.Candidates) || len(off.Watch) != len(on.Watch) {
		t.Errorf("候选口径漂移：开=%d/%d empty=%v，关=%d/%d empty=%v",
			len(on.Candidates), on.ScoredTotal, on.Empty,
			len(off.Candidates), off.ScoredTotal, off.Empty)
	}

	// ③ 已落库的告警 detail 逐字相同（At 是墙上时钟，不参与比对）。
	for _, tc := range []struct{ name, got, want string }{
		{"subject", onTrace.Subject, offTrace.Subject},
		{"outcome", onTrace.Outcome, offTrace.Outcome},
		{"detail", onTrace.Detail, offTrace.Detail},
	} {
		if tc.got != tc.want {
			t.Errorf("开关改动了既有 %s 文案:\n开=%q\n关=%q", tc.name, tc.got, tc.want)
		}
	}
	if strings.Contains(onTrace.Detail, "板块影子") {
		t.Errorf("板块影子摘要不应混进 SCREEN_EMPTY 告警（关闸日之外也只走 screen:topn 一个出口）: %s", onTrace.Detail)
	}

	// ④ 唯一差异：影子清单只在开启时存在，且条数与 sector drops 相等。
	if off.SectorShadow != nil {
		t.Errorf("关闭时 SectorShadow 应为 nil，实际 %d 条", len(off.SectorShadow))
	}
	if len(on.SectorShadow) != sectorOn.Drops[reasonSectorOut] {
		t.Errorf("SectorShadow %d 条应与 sector drops %d 条相等", len(on.SectorShadow), sectorOn.Drops[reasonSectorOut])
	}
	// 摘要是清单的函数：清单为空 ⇒ 无摘要；清单有内容 ⇒ 摘要报出同一总数。
	if (&Report{SectorShadow: off.SectorShadow}).SectorShadowBrief() != "" {
		t.Error("关闭态摘要不得有内容")
	}
	if got := on.SectorShadowBrief(); !strings.Contains(got, fmt.Sprintf("板块影子 %d 只", len(on.SectorShadow))) {
		t.Errorf("摘要总数与实际清单条数不符: %q / %d 条", got, len(on.SectorShadow))
	}
}

// stageDiff 逐级比对进出与淘汰原因分布，返回可读差异（空串表示完全一致）。
func stageDiff(a, b []StageStat) string {
	var out []string
	if len(a) != len(b) {
		return fmt.Sprintf("级数不同：关 %d 级 / 开 %d 级", len(a), len(b))
	}
	for i := range a {
		if a[i].Slug != b[i].Slug || a[i].Name != b[i].Name || a[i].In != b[i].In || a[i].Out != b[i].Out {
			out = append(out, fmt.Sprintf("第 %d 级不同：关=%+v 开=%+v", i+1, a[i], b[i]))
			continue
		}
		if len(a[i].Drops) != len(b[i].Drops) {
			out = append(out, fmt.Sprintf("第 %d 级淘汰原因数不同：关=%v 开=%v", i+1, a[i].Drops, b[i].Drops))
			continue
		}
		for k, v := range a[i].Drops {
			if b[i].Drops[k] != v {
				out = append(out, fmt.Sprintf("第 %d 级 %s 计数不同：关=%d 开=%d", i+1, k, v, b[i].Drops[k]))
			}
		}
	}
	return strings.Join(out, "\n")
}
