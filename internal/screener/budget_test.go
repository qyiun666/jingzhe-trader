package screener

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestBudgetPerSlotTakesStricter 可用资金这一级的预算线 = 现金均分与风控单票上限取严。
//
// 这是 P0 口径修复的正面断言：20261008 现金 11833.80 / 2 槽 = 5916.90 元，而弱势试探
// 的单票上限是 1183.38 元，漏斗按前者放行、决策链按后者砍单，于是 4/20 候选
// （002675/600216/600763/688050）是"选出来就永远买不起"的票。
func TestBudgetPerSlotTakesStricter(t *testing.T) {
	const cash = model.Fen(1183380) // 11833.80 元
	cases := []struct {
		name string
		b    Budget
		want model.Fen
	}{
		{"弱势上限更严则用上限", Budget{Cash: cash, Slots: 2, CapFen: model.Fen(118338)}, model.Fen(118338)},
		{"均分更严则用均分", Budget{Cash: cash, Slots: 2, CapFen: model.Fen(900000)}, model.Fen(591690)},
		{"无上限口径退化成均分", Budget{Cash: cash, Slots: 2}, model.Fen(591690)},
		{"上限为 0 视为未给口径", Budget{Cash: cash, Slots: 2, CapFen: 0}, model.Fen(591690)},
		{"无现金不放行", Budget{Cash: 0, Slots: 2, CapFen: model.Fen(118338)}, 0},
		{"无槽位不放行", Budget{Cash: cash, Slots: 0, CapFen: model.Fen(118338)}, 0},
		{"负槽位不放行", Budget{Cash: cash, Slots: -1, CapFen: model.Fen(118338)}, 0},
	}
	for _, c := range cases {
		if got := c.b.perSlot(); got != c.want {
			t.Errorf("%s: 期望预算 %d 分，实际 %d 分", c.name, c.want, got)
		}
	}
}

// TestAffordableStageUsesCapLine 预算线收口后，一手成本落在均分与上限之间的股票
// 必须被判"买不起"——这正是修复前后候选构成的差异所在。
//
// 注意 affordableStage 的入参是**每股价格（分）**，一手成本由它 ×100 现算。
func TestAffordableStageUsesCapLine(t *testing.T) {
	b := Budget{Cash: model.Fen(1183380), Slots: 2, CapFen: model.Fen(118338)}
	line := b.perSlot()
	// 12.63 元/股 → 一手 1263 元：均分口径买得起（< 5916.90），上限口径买不起（> 1183.38）。
	if ok, reason := affordableStage(model.Fen(1263), line); ok {
		t.Errorf("一手 1263 元在 1183.38 元预算线下应被拒，实际放行")
	} else if reason != reasonUnaffordable {
		t.Errorf("拒绝原因应为 %q，实际 %q", reasonUnaffordable, reason)
	}
	// 11.83 元/股 → 一手 1183 元：卡线内，必须放行（边界不能被多收一分钱砍掉）。
	if ok, reason := affordableStage(model.Fen(1183), line); !ok {
		t.Errorf("一手 1183 元小于预算线 1183.38 元应放行，实际被拒：%q", reason)
	}
	// 11.84 元/股 → 一手 1184 元：刚过线，必须被拒。
	if ok, _ := affordableStage(model.Fen(1184), line); ok {
		t.Errorf("一手 1184 元超过预算线 1183.38 元应被拒，实际放行")
	}
	// 收口前这条线是 5916.90 元：同一只票在旧口径下会放行，这就是 bug 的形态。
	if ok, _ := affordableStage(model.Fen(1263), b.Cash/model.Fen(b.Slots)); !ok {
		t.Errorf("旧的均分口径应放行一手 1263 元（用于对照，证明差异来自收口）")
	}
}

// TestBudgetNoteDisclosesBindingLine 预算线进日志：归因时不该再靠现金和持仓数反推。
func TestBudgetNoteDisclosesBindingLine(t *testing.T) {
	weak := budgetNote(Budget{Cash: model.Fen(1183380), Slots: 2, CapFen: model.Fen(118338)})
	if !strings.Contains(weak, "风控单票上限 1183.38") || !strings.Contains(weak, "现金均分 5916.90") {
		t.Errorf("上限收口时日志应同时给出两条线，实际: %q", weak)
	}
	plain := budgetNote(Budget{Cash: model.Fen(1183380), Slots: 2})
	if !strings.Contains(plain, "现金均分口径") || strings.Contains(plain, "风控单票上限") {
		t.Errorf("未给单票上限时应如实说均分口径，实际: %q", plain)
	}
}
