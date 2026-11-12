package scheduler

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
)

// 影子三态在**数据面**上的可分性（派工 20261112 §三）。
//
// screener 包测的是 SectorShadowBrief() 返回什么串；本文件测的是那一串穿过
// writeScreenSnapshot 之后 run_trace.detail 里到底有没有那个键 —— 组长实际读的就是这一行，
// 11-10 的坑也正是在这一行上看不见。三格必须给出三种可分辨的载荷：
//
//	① 开关关闭（清单 nil）        → 无 sector_shadow_brief 键，整段与旧载荷逐字节相同
//	② 关闸互斥（RegimeClosed）    → 无键（维持现状）
//	③ 已开启 + 当日 0 淘汰        → 有键，值含「板块影子 0 只」
//	   已开启 + 有清单             → 有键，值为总数句（与 11-12 实测同型）
//
// 用例直接用 screener.Report 当输入，把"报告 → 摘要 → 载荷"这条真实链路整段锁住。
func TestWriteScreenSnapshotSectorShadowTriState(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/tristate.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	d := Deps{Store: st}
	rp := risk.DefaultParams(model.FromFloat(10000))
	cands := []model.Candidate{{
		TsCode: "600000.SH", Name: "甲", Industry: "银行", Rank: 1, PoolSize: 20,
		Score: 88.5, Factors: model.FactorScore{Momentum: 90, Value: 80, LowVol: 70, Liquidity: 60},
		Close: 1234, Mom: 0.08, SectorMom: 0.05,
	}}

	// 四种 Report 形状 → 四种 brief → 写同一天（重跑覆盖），逐格取回原始 detail。
	detail := func(t *testing.T, rep *screener.Report) string {
		t.Helper()
		if err := writeScreenSnapshot(ctx, d, "20261112", cands, rp, rep.SectorShadowBrief()); err != nil {
			t.Fatalf("写快照失败: %v", err)
		}
		return topNDetail(t, st, "20261112")
	}

	// ① 开关关闭：清单保持 nil。
	off := detail(t, &screener.Report{})
	// ② 互斥：开关开着但当日关闸（清单 nil，且即使被填也不能出现）。
	closed := detail(t, &screener.Report{RegimeClosed: true, SectorShadow: []model.Candidate{}})
	// ③ 已开启 + 当日 0 淘汰：非 nil 空清单。
	zero := detail(t, &screener.Report{SectorShadow: []model.Candidate{}})
	// ④ 已开启 + 有清单：与 11-12 首批实盘原文同型。
	listed := detail(t, &screener.Report{SectorShadow: []model.Candidate{{
		TsCode: "600000.SH", Name: "浦发银行", Industry: "银行",
		Close: model.Fen(1179), SectorShadowFiltered: true,
	}}})

	// 无键的两格：不得出现影子字段，且彼此逐字节相同（与旧载荷的比较在
	// TestWriteScreenSnapshotSectorShadowField 已经做过，这里只补"②不再与③同形"这一头）。
	for _, tc := range []struct{ name, got string }{{"①开关关闭", off}, {"②关闸互斥", closed}} {
		if strings.Contains(tc.got, "sector_shadow") {
			t.Errorf("%s：载荷不得写出影子键，实际=%s", tc.name, tc.got)
		}
		if strings.Contains(tc.got, "板块影子") {
			t.Errorf("%s：detail 不得出现影子文案，实际=%s", tc.name, tc.got)
		}
	}
	if off != closed {
		t.Errorf("①与②必须同形（都无键）：\n①=%s\n②=%s", off, closed)
	}

	// 有键的两格：值必须能自证开关已开启，且 ③ 与 ① 明确不同形 —— 这就是本工单的全部目的。
	keys := func(name, raw string) map[string]json.RawMessage {
		t.Helper()
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("%s 载荷解析失败: %v", name, err)
		}
		if _, ok := m["sector_shadow_brief"]; !ok {
			t.Fatalf("%s：载荷缺少 sector_shadow_brief 键（三态又塌回同形了）: %s", name, raw)
		}
		if len(m) != 5 {
			t.Errorf("%s：应恰好 5 个键，实际 %d: %s", name, len(m), raw)
		}
		return m
	}
	var zeroBrief, listedBrief string
	raw := keys("③0淘汰", zero)["sector_shadow_brief"]
	if err := json.Unmarshal(raw, &zeroBrief); err != nil {
		t.Fatalf("解析 ③ 摘要失败: %v", err)
	}
	if !strings.Contains(zeroBrief, "板块影子 0 只") || !strings.Contains(zeroBrief, "观测开关已开启") {
		t.Errorf("③ 摘要须同时报出 0 只与开关已开启，实际=%q", zeroBrief)
	}
	if zeroBrief == "" {
		t.Error("③ 摘要不得为空串（空串会被 omitempty 吞掉，键就不存在了）")
	}
	raw = keys("④有清单", listed)["sector_shadow_brief"]
	if err := json.Unmarshal(raw, &listedBrief); err != nil {
		t.Fatalf("解析 ④ 摘要失败: %v", err)
	}
	if !strings.HasPrefix(listedBrief, "板块影子 1 只（sector 级淘汰，未参与打分，漏斗输入序）：") {
		t.Errorf("④ 摘要与 11-12 实盘原文不同型，实际=%q", listedBrief)
	}
	// 三格互不相同：①/② 无键、③ 空集句、④ 总数句。
	if zeroBrief == listedBrief {
		t.Errorf("③ 与 ④ 摘要相同，空集与有清单不可分: %q", zeroBrief)
	}

	// 「只追加」的字节级证明：新键排在末尾，① 去掉右花括号后必须是 ③/④ 的逐字节前缀。
	if !strings.HasSuffix(off, "}") {
		t.Fatalf("① detail 不是 JSON 对象: %s", off)
	}
	prefix := strings.TrimSuffix(off, "}")
	for _, tc := range []struct{ name, got string }{{"③0淘汰", zero}, {"④有清单", listed}} {
		if !strings.HasPrefix(tc.got, prefix) || !strings.HasSuffix(tc.got, "}") {
			t.Errorf("%s：既有四键被改动（应为 ① 的末尾追加）:\n①=%s\n%s=%s", tc.name, off, tc.name, tc.got)
		}
	}

	// 回读侧：③ 的载荷解析成新结构后，影子字段读得出、候选字段与 ① 完全一致。
	var snapOff, snapZero model.ScreenSnapshot
	if err := json.Unmarshal([]byte(off), &snapOff); err != nil {
		t.Fatalf("解析 ① 失败: %v", err)
	}
	if err := json.Unmarshal([]byte(zero), &snapZero); err != nil {
		t.Fatalf("解析 ③ 失败: %v", err)
	}
	if snapOff.SectorShadowBrief != "" {
		t.Errorf("① 回读影子字段应为空，实际=%q", snapOff.SectorShadowBrief)
	}
	if snapZero.SectorShadowBrief != zeroBrief {
		t.Errorf("③ 回读失真: %q vs %q", snapZero.SectorShadowBrief, zeroBrief)
	}
	if len(snapZero.Candidates) != len(snapOff.Candidates) ||
		snapZero.Candidates[0] != snapOff.Candidates[0] {
		t.Errorf("③ 改动了 candidates: %+v vs %+v", snapZero.Candidates, snapOff.Candidates)
	}
}

// TestScreenSnapshotCallSiteDerivesShadowBrief 接线锁：调用 writeScreenSnapshot 的那个函数
// 体内必须存在 SectorShadowBrief 的取用（写成选择器，允许先落进局部变量再传参）。
//
// 为什么要有这一条：上面所有载荷用例都是**直接调 writer**，它们能证明 writer 正确，
// 却证明不了"当日那行 detail 里真的会有这个键"。11-10 的故障形态正是产出侧齐备、
// 消费侧零调用，而彼时的测试全绿 —— 本工单要消灭的"看不见 ⇒ 判不出"不能只消灭一半。
// 变异验证：把调用点的实参换成 "" 或 rep.ShadowBrief()，本用例必须红。
func TestScreenSnapshotCallSiteDerivesShadowBrief(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "pipeline.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 pipeline.go 失败（接线锁不能空跑）: %v", err)
	}
	checked := 0
	ast.Inspect(file, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			return true
		}
		var callsWriter, takesBrief bool
		ast.Inspect(fd.Body, func(m ast.Node) bool {
			switch e := m.(type) {
			case *ast.CallExpr:
				if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "writeScreenSnapshot" {
					callsWriter = true
				}
			case *ast.SelectorExpr:
				if e.Sel.Name == "SectorShadowBrief" {
					takesBrief = true
				}
			}
			return true
		})
		if callsWriter {
			checked++
			if !takesBrief {
				t.Errorf("%s 调用了 writeScreenSnapshot 却没有取用 SectorShadowBrief ⇒ 影子键不会出现在当日 detail",
					fd.Name.Name)
			}
		}
		return true
	})
	if checked == 0 {
		t.Fatal("pipeline.go 里找不到 writeScreenSnapshot 调用点：接线锁本身失效了（改名/搬走也要有人发现）")
	}
}
