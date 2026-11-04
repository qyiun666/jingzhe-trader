package screener

import (
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
)

// TestLotWindowRejectsUnorderableCandidates 整手量化空洞（20261104 策略 B1）。
//
// 缺陷形态：漏斗过去只判"一手 ≤ 单票上限"，而风控在金额定稿后还判"金额 ≥ 单笔下限"，
// 申报量又必须是 100 股的整数倍。弱势档的窗口 [880.34, 1,173.78] 只有 293 元宽，
// 整手步距 100×price，于是大量价格在窗口里一个整数倍都落不进去。
//
// 数据取自 11-03 实跑 screen:topn 的 20 只候选（slot_fen=117378）：其中 13 只不可下单，
// 11-04 的决策留痕里模型已经自己写出了这件事（九芝堂 8.37 元：
// "一手843元低于单笔下限880.34元，200股又超单票上限…无法合规建仓"）。
// 这条测试的作用是把"实测 13/20"钉成一个数字，改回旧口径时它会红。
func TestLotWindowRejectsUnorderableCandidates(t *testing.T) {
	capFen := model.Fen(117378) // 1,173.78 元（弱势档单票上限 10%）
	minFen := risk.MinAmountFor(capFen, true)
	if minFen != model.Fen(88033) && minFen != model.Fen(88034) {
		t.Fatalf("弱势档单笔下限应为约 880.34 元，实际 %s 元（口径变了必须同步下面的表）", minFen)
	}

	orderable := []int64{345, 513, 885, 1014, 528, 983, 1051}          // 民生/仁和/川能/九洲/宁医/英特/德昌
	hollow := []int64{833, 627, 395, 735, 702, 693, 628, 621, 622, 713, 651, 837, 436} // 其余 13 只

	for _, price := range orderable {
		if ok, why := affordableStage(model.Fen(price), capFen, minFen); !ok {
			t.Errorf("%d 分/股（一手 %.2f 元）应有合法整手，实际被拒：%q", price, float64(price), why)
		}
	}
	for _, price := range hollow {
		ok, why := affordableStage(model.Fen(price), capFen, minFen)
		if ok {
			t.Errorf("%d 分/股（一手 %.2f 元）无任何整手落进 [%.2f, %.2f] 元，实际放行",
				price, float64(price), minFen.Float(), capFen.Float())
			continue
		}
		if why != ReasonLotWindow {
			t.Errorf("%d 分/股 拒绝原因应为 %q，实际 %q", price, ReasonLotWindow, why)
		}
	}
	if len(orderable)+len(hollow) != 20 {
		t.Fatalf("样本表必须覆盖 11-03 的 20 只候选，实际 %d 只", len(orderable)+len(hollow))
	}
}

// TestLotWindowETFCostGranularity ETF 补口正是因为这个口径才成立：一手几十到一百多元，
// 整手步距远小于窗口宽度，任何名义额都落得进去。
func TestLotWindowETFCostGranularity(t *testing.T) {
	capFen := model.Fen(117378)
	minFen := risk.MinAmountFor(capFen, true)
	for _, price := range []int64{62, 65, 74, 79, 126, 134} { // 白名单 7 只的 11-04 现价（分）
		if ok, why := affordableStage(model.Fen(price), capFen, minFen); !ok {
			t.Errorf("ETF 一手 %d 分应可下单，实际被拒：%q", price, why)
		}
	}
}

// TestBoundsFlagsEmptyWindow 下界高于上界（配置自相矛盾）时不得把候选池筛光：
// 那会把"配置坏了"显示成"今天没有符合条件的票"。置 0 退化 + 出声是唯一可诊断的形态。
func TestBoundsFlagsEmptyWindow(t *testing.T) {
	b := Budget{Cash: model.Fen(1173783), Slots: 2, CapFen: model.Fen(117378), MinFen: model.Fen(500000)}
	capFen, minFen, empty := b.bounds()
	if !empty {
		t.Fatalf("下限 5000 > 上限 1173.78 应报空集旗标")
	}
	if minFen != 0 || capFen != model.Fen(117378) {
		t.Errorf("空集时应退化为只判上界，实际 cap=%s min=%s", capFen, minFen)
	}
	if ok, _ := affordableStage(model.Fen(833), capFen, minFen); !ok {
		t.Errorf("矛盾配置下不得静默清空候选（旧行为保持）")
	}
}

// TestAffordableStageRejectsBadPrice 价格非正/无上限口径时不放行（宁缺勿滥）。
func TestAffordableStageRejectsBadPrice(t *testing.T) {
	capFen, minFen := model.Fen(117378), risk.MinAmountFor(model.Fen(117378), true)
	if ok, _ := affordableStage(0, capFen, minFen); ok {
		t.Errorf("价格 0 不应放行")
	}
	if ok, _ := affordableStage(model.Fen(513), 0, minFen); ok {
		t.Errorf("无单票上限口径不应放行")
	}
}

// TestBudgetNoteDisclosesFloorLine 下界必须出现在日志行里，否则事后分不出
// "被筛掉"和"根本没法按整手买"。
func TestBudgetNoteDisclosesFloorLine(t *testing.T) {
	weak := budgetNote(Budget{Cash: model.Fen(1173783), Slots: 2, CapFen: model.Fen(117378),
		MinFen: risk.MinAmountFor(model.Fen(117378), true)})
	if !contains(weak, "单笔下限") {
		t.Errorf("给了下界口径时日志应写出它，实际: %q", weak)
	}
	plain := budgetNote(Budget{Cash: model.Fen(1173783), Slots: 2, CapFen: model.Fen(117378)})
	if contains(plain, "单笔下限") {
		t.Errorf("未给下界口径时不该凭空造一条线，实际: %q", plain)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
