package risk

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// batchState 构造批次核算的账户状态。
func batchState(totalAssetYuan, cashYuan, positionsMVYuan float64, posCount int, held ...string) AccountState {
	heldSet := make(map[string]bool, len(held))
	heldMV := make(map[string]model.Fen, len(held))
	for _, c := range held {
		heldSet[c] = true
		heldMV[c] = 0 // 敞口未指定时按 0 起算（额度=单票上限）；需要精确额度用 addOnState
	}
	return AccountState{
		TotalAsset:    model.Fen(totalAssetYuan * 100),
		Cash:          model.Fen(cashYuan * 100),
		PositionsMV:   model.Fen(positionsMVYuan * 100),
		PositionCount: posCount,
		HeldCodes:     heldSet,
		HeldMV:        heldMV,
	}
}

// addOnState 已持有 code 且其现有敞口为 heldMVYuan（元）的账户状态，用于加仓额度用例。
func addOnState(totalAssetYuan, cashYuan, positionsMVYuan float64, code string, heldMVYuan float64) AccountState {
	st := batchState(totalAssetYuan, cashYuan, positionsMVYuan, 1, code)
	st.HeldMV[code] = model.FromFloat(heldMVYuan)
	return st
}

// g1Params 按总资产解析出的生效参数。10 万元：单票 4 万 / 总仓 9 万 / 最大持仓 6 / 置信度 0.55。
//
// 这里刻意把摩擦闸关掉（ExpectedExcessPct=1，即"预期超额 100%"，任何摩擦都盖得过）：
// DefaultParams 的生效默认值是 0 = 无显著归因证据即全拦（20261103 定参①），
// 那是**另一条独立规则**，由 TestFrictionGate* 专门覆盖。本文件的用例测的是
// 批次累计总仓、行业上限、持仓数、金额上下限、加仓额度——让它们各自先撞上
// 摩擦闸就等于什么都没测到。闸的开与关在下面的专项用例里断言，不在这里顺带。
func g1Params(totalAssetYuan float64) RiskParams {
	p := DefaultParams(model.Fen(int64(totalAssetYuan * 100)))
	p.ExpectedExcessPct = 1
	return p
}

// intent 构造一笔"决策要求投入 wantYuan 元"的买入意向（缺省要满）。
func intent(code string, priceYuan, wantYuan, conf float64) BuyIntent {
	return BuyIntent{
		TsCode: code, Name: code, RefPrice: model.FromFloat(priceYuan),
		Confidence: conf, WantFen: model.FromFloat(wantYuan),
	}
}

// TestNoBatchOverTotalPositionLimit 对应验收 #7（历史 P0 bug 回归）：
// 同一批 3 笔买入，单笔均不超单票上限（4 万），但合计 12 万超总仓位上限（9 万）
// → 第 3 笔必须被拒，且 reject_rule == max_total_position。
func TestNoBatchOverTotalPositionLimit(t *testing.T) {
	m := NewManager(g1Params(100000))
	intents := []BuyIntent{
		intent("sh600001", 10, 40000, 0.9),
		intent("sh600002", 10, 40000, 0.9),
		intent("sh600003", 10, 40000, 0.9),
	}
	got := m.CheckBatch(intents, batchState(100000, 100000, 0, 0))
	if len(got) != 3 {
		t.Fatalf("裁决数=%d, 期望 3", len(got))
	}
	if !got[0].Approved || !got[1].Approved {
		t.Errorf("前两笔应通过: %+v, %+v", got[0], got[1])
	}
	if got[2].Approved {
		t.Fatalf("第 3 笔应被拒（批次累计超总仓位）: %+v", got[2])
	}
	if got[2].RejectRule != RuleMaxTotalPosition {
		t.Errorf("reject_rule=%q, 期望 %q", got[2].RejectRule, RuleMaxTotalPosition)
	}
	if !strings.Contains(got[2].RejectMsg, "总仓位上限") {
		t.Errorf("reject_msg 未写明原因: %q", got[2].RejectMsg)
	}
	if got[0].Amount != model.FromFloat(40000) {
		t.Errorf("首笔金额=%s, 期望 40000.00", got[0].Amount)
	}
}

// TestBatchRejectionWithExistingPositions 已有持仓市值计入累计基数：1 万持仓 + 3×4 万在第 3 笔被拒。
func TestBatchRejectionWithExistingPositions(t *testing.T) {
	m := NewManager(g1Params(100000)) // 总仓上限 9 万
	intents := []BuyIntent{
		intent("sh600001", 10, 40000, 0.9),
		intent("sh600002", 10, 40000, 0.9),
		intent("sh600003", 10, 40000, 0.9),
	}
	got := m.CheckBatch(intents, batchState(100000, 90000, 10000, 1, "sz000009"))
	if !got[0].Approved || !got[1].Approved {
		t.Errorf("前两笔应通过: %+v %+v", got[0], got[1])
	}
	if got[2].Approved || got[2].RejectRule != RuleMaxTotalPosition {
		t.Errorf("第 3 笔应因 max_total_position 被拒: %+v", got[2])
	}
}

// TestIndustryCapDefaultAllowsTwo 默认档同行业最多 2 只：第 3 只开新仓被行业上限拒绝，
// 前两只照常通过；已持有同行业 1 只时，只允许再加 1 只。
func TestIndustryCapDefaultAllowsTwo(t *testing.T) {
	p := g1Params(100000) // MaxPerIndustry=2
	if p.MaxPerIndustry != 2 {
		t.Fatalf("默认档行业上限=%d，期望 2", p.MaxPerIndustry)
	}
	m := NewManager(p)
	intents := []BuyIntent{
		{TsCode: "a", Industry: "电子", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(10000)},
		{TsCode: "b", Industry: "电子", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(10000)},
		{TsCode: "c", Industry: "电子", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(10000)},
	}
	got := m.CheckBatch(intents, batchState(100000, 100000, 0, 0))
	if !got[0].Approved || !got[1].Approved {
		t.Fatalf("同行业前两只应通过: %+v %+v", got[0], got[1])
	}
	if got[2].Approved || got[2].RejectRule != RuleIndustryCap {
		t.Fatalf("同行业第 3 只应被行业上限拒绝: %+v", got[2])
	}

	// 已持有 1 只电子：还能开第 2 只，第 3 只拒绝。
	st := batchState(100000, 90000, 10000, 1, "held")
	st.HeldIndustry = map[string]string{"held": "电子"}
	intents = []BuyIntent{
		{TsCode: "d", Industry: "电子", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(10000)},
		{TsCode: "e", Industry: "电子", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(10000)},
	}
	got = m.CheckBatch(intents, st)
	if !got[0].Approved {
		t.Fatalf("已有 1 只同行业时第 2 只应通过: %+v", got[0])
	}
	if got[1].Approved || got[1].RejectRule != RuleIndustryCap {
		t.Fatalf("已有 2 只同行业时第 3 只应拒绝: %+v", got[1])
	}
}

// TestIndustryCapWeakAndUnknown 弱势档同行业最多 1 只；行业未知降级放行并留 Note；
// ETF 不占个股行业名额。
func TestIndustryCapWeakAndUnknown(t *testing.T) {
	weak := WeakParams(g1Params(100000), 0.20, 0.10)
	// 本用例测的是行业上限，不是买入闸：WeakParams 按定参默认会把 BuysBlocked 置起来
	// （弱势期不下买单），这里显式放开才能走到行业上限那条判定。
	// 闸本身由 TestWeakParamsBlocksBuysByDefault 与 TestBuysBlockedRejectsEveryIntent 覆盖。
	weak.BuysBlocked = false
	if weak.MaxPerIndustry != 1 {
		t.Fatalf("弱势档行业上限=%d，期望 1", weak.MaxPerIndustry)
	}
	m := NewManager(weak)
	intents := []BuyIntent{
		{TsCode: "a", Industry: "电子", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(5000)},
		{TsCode: "b", Industry: "电子", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(5000)},
	}
	got := m.CheckBatch(intents, batchState(100000, 100000, 0, 0))
	if !got[0].Approved {
		t.Fatalf("弱势档同行业第 1 只应通过: %+v", got[0])
	}
	if got[1].Approved || got[1].RejectRule != RuleIndustryCap {
		t.Fatalf("弱势档同行业第 2 只应拒绝: %+v", got[1])
	}

	// 行业未知：两只都放行，且各自带 Note（不静默阻断）。
	m = NewManager(g1Params(100000))
	got = m.CheckBatch([]BuyIntent{
		{TsCode: "x", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(10000)},
		{TsCode: "y", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(10000)},
	}, batchState(100000, 100000, 0, 0))
	if !got[0].Approved || !got[1].Approved {
		t.Fatalf("行业未知应降级放行: %+v %+v", got[0], got[1])
	}
	if !strings.Contains(got[0].Note, "行业未知") {
		t.Errorf("行业未知应留 Note，实际 %+v", got[0])
	}

	// ETF 不占行业名额：同一行业已满 2 只时，ETF 候选仍可通过。
	st := batchState(100000, 80000, 20000, 2, "h1", "h2")
	st.HeldIndustry = map[string]string{"h1": "电子", "h2": "电子"}
	got = NewManager(g1Params(100000)).CheckBatch([]BuyIntent{
		{TsCode: "588000.SH", Industry: "电子", IsETF: true, RefPrice: model.FromFloat(1), Confidence: 0.9, WantFen: model.FromFloat(5000)},
	}, st)
	if !got[0].Approved {
		t.Fatalf("ETF 不应受个股行业上限约束: %+v", got[0])
	}
}

// TestCheckBatchAllRules 每条否决规则逐一验证（100% 留痕的字段取值契约）。
func TestCheckBatchAllRules(t *testing.T) {
	p := g1Params(100000)
	cases := []struct {
		name      string
		params    RiskParams
		intents   []BuyIntent
		state     AccountState
		wantRule  string
		wantIndex int
	}{
		{
			name: "禁开新仓", params: withAllowNew(p, false),
			intents:  []BuyIntent{intent("a", 10, 40000, 0.9)},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleAllowNewPosition, wantIndex: 0,
		},
		{
			name: "置信度不足", params: p,
			intents:  []BuyIntent{intent("a", 10, 40000, 0.50)},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleMinConfidence, wantIndex: 0,
		},
		{
			name: "加仓额度已满", params: p,
			// 已持有 a，其敞口已达单票上限 4 万 → 无可加额度
			intents:  []BuyIntent{intent("a", 10, 40000, 0.9)},
			state:    addOnState(100000, 100000, 40000, "a", 40000),
			wantRule: RuleSinglePositionFull, wantIndex: 0,
		},
		{
			name: "持仓数达上限", params: withMaxPos(p, 1),
			intents:  []BuyIntent{intent("b", 10, 40000, 0.9)},
			state:    batchState(100000, 100000, 0, 1, "a"),
			wantRule: RuleMaxPositions, wantIndex: 0,
		},
		{
			name: "一手买不起", params: p,
			intents:  []BuyIntent{intent("a", 900, 40000, 0.9)}, // 一手 9 万 > 单票上限 4 万
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleLotUnaffordable, wantIndex: 0,
		},
		{
			name: "单笔金额低于下限", params: withMinAmount(p, model.FromFloat(60000)),
			intents:  []BuyIntent{intent("a", 10, 6000, 0.9)},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleMinAmount, wantIndex: 0,
		},
		{
			name: "参考价非法", params: p,
			intents:  []BuyIntent{intent("a", 0, 40000, 0.9)},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleIllegalPrice, wantIndex: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewManager(tc.params).CheckBatch(tc.intents, tc.state)
			d := got[tc.wantIndex]
			if d.Approved {
				t.Fatalf("应被否决: %+v", d)
			}
			if d.RejectRule != tc.wantRule {
				t.Errorf("reject_rule=%q, 期望 %q", d.RejectRule, tc.wantRule)
			}
			if d.RejectMsg == "" {
				t.Errorf("reject_msg 不得为空（否决必须可解释）")
			}
		})
	}
}

// TestCheckBatchBatchMaxPositionsInFlight 批内累计持仓数：上限 2 时第 3 笔新仓被拒。
func TestCheckBatchBatchMaxPositionsInFlight(t *testing.T) {
	p := withMaxPos(g1Params(100000), 2)
	m := NewManager(p)
	intents := []BuyIntent{
		intent("a", 10, 40000, 0.9),
		intent("b", 10, 40000, 0.9),
		intent("c", 10, 40000, 0.9),
	}
	got := m.CheckBatch(intents, batchState(100000, 100000, 0, 0))
	if !got[0].Approved || !got[1].Approved {
		t.Errorf("前两笔应通过: %+v %+v", got[0], got[1])
	}
	if got[2].Approved || got[2].RejectRule != RuleMaxPositions {
		t.Errorf("第 3 笔应因 max_positions 被拒: %+v", got[2])
	}
}

// TestWantFenTruncatedByCaps 决策要的钱必须被硬截断：这是"LLM 决定数量、风控只做截断"的接缝。
func TestWantFenTruncatedByCaps(t *testing.T) {
	p := g1Params(100000) // 单票上限 4 万
	cases := []struct {
		name       string
		wantYuan   float64
		cashYuan   float64
		wantAmount model.Fen
	}{
		{"要满则给到单票上限", 999999, 100000, model.FromFloat(40000)},
		{"要得少就按要的给", 12000, 100000, model.FromFloat(12000)},
		{"现金不足则按现金截断", 40000, 15000, model.FromFloat(15000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewManager(p).CheckBatch(
				[]BuyIntent{intent("a", 10, tc.wantYuan, 0.9)},
				batchState(100000, tc.cashYuan, 0, 0))
			if !got[0].Approved {
				t.Fatalf("应通过: %+v", got[0])
			}
			if got[0].Amount != tc.wantAmount {
				t.Errorf("金额=%s, 期望 %s", got[0].Amount, tc.wantAmount)
			}
			if got[0].Qty%model.LotShares != 0 {
				t.Errorf("股数 %d 不是整手", got[0].Qty)
			}
		})
	}
}

// TestMinAmountFloorScalesWithCapital 单笔金额下限必须随资金缩放：
// 5000 元的绝对下限是为压佣金设的，两万元账户的单票上限本身不到一万元，
// 按绝对值卡会让每一个候选都判"金额过小"，当日一张单都出不来。
func TestMinAmountFloorScalesWithCapital(t *testing.T) {
	cases := []struct {
		totalYuan float64
		wantFen   model.Fen
	}{
		{20000, model.FromFloat(4000)},  // 单票上限 8000 → 下限收口到一半
		{50000, model.FromFloat(5000)},  // 单票上限 2 万，绝对下限 5000 仍生效
		{200000, model.FromFloat(5000)}, // 大账户保持 5000 元
	}
	for _, tc := range cases {
		p := g1Params(tc.totalYuan)
		if got := p.MinAmountFloor(); got != tc.wantFen {
			t.Errorf("总资产 %.0f 元时生效下限=%s, 期望 %s", tc.totalYuan, got, tc.wantFen)
		}
	}
}

// TestTargetQty 整手取整边界。
func TestTargetQty(t *testing.T) {
	cases := []struct {
		amount, price model.Fen
		want          model.Qty
	}{
		{model.FromFloat(10000), model.FromFloat(10), 1000},
		{model.FromFloat(10050), model.FromFloat(10), 1000}, // 向下取整
		{model.FromFloat(99), model.FromFloat(10), 0},       // 不足一手
		{model.FromFloat(10000), 0, 0},                      // 非法价格
	}
	for _, tc := range cases {
		if got := TargetQty(tc.amount, tc.price); got != tc.want {
			t.Errorf("TargetQty(%d, %d)=%d, 期望 %d", tc.amount, tc.price, got, tc.want)
		}
	}
}

// ---------- 参数变体辅助 ----------

func withAllowNew(p RiskParams, allow bool) RiskParams {
	p.AllowNewPosition = allow
	return p
}

func withMaxPos(p RiskParams, n int) RiskParams {
	p.MaxPositions = n
	return p
}

func withMinAmount(p RiskParams, fen model.Fen) RiskParams {
	p.MinSingleAmountFen = fen
	return p
}

// TestAddOnAllowsHeldCode 加仓语义：已持有的标的可再买（不再是 already_holding 拒绝），
// 不占新的持仓名额，且受"单票上限 − 现有敞口"的剩余额度约束。
func TestAddOnAllowsHeldCode(t *testing.T) {
	p := g1Params(100000) // 单票上限 4 万 / 最大持仓 6

	// 1) 持仓名额已满（6/6），但加仓不占名额 → 仍可通过
	full := batchState(100000, 100000, 0, 6, "a")
	full.HeldMV["a"] = model.FromFloat(10000) // 已占 1 万，还剩 3 万额度
	got := NewManager(p).CheckBatch([]BuyIntent{intent("a", 10, 40000, 0.9)}, full)
	if !got[0].Approved {
		t.Fatalf("持仓名额已满时加仓应通过（不占新名额），实际被拒: %+v", got[0])
	}
	// 剩余额度 3 万 → 要 4 万被截断到 3 万 → 3000 股×10 元
	if got[0].Amount != model.FromFloat(30000) {
		t.Errorf("加仓金额=%s, 期望 30000.00（单票上限 4 万 − 已占 1 万）", got[0].Amount)
	}

	// 2) 加仓额度用尽（已占满单票上限）→ 拒，规则是 single_position_full
	full2 := addOnState(100000, 100000, 40000, "a", 40000)
	got2 := NewManager(p).CheckBatch([]BuyIntent{intent("a", 10, 40000, 0.9)}, full2)
	if got2[0].Approved || got2[0].RejectRule != RuleSinglePositionFull {
		t.Errorf("额度用尽应因 single_position_full 被拒: %+v", got2[0])
	}

	// 3) 批内先通过一笔新仓，后续同码视为加仓（不占第二名额）
	p2 := withMaxPos(p, 1)
	batch := NewManager(p2).CheckBatch([]BuyIntent{
		intent("a", 10, 30000, 0.9), // 新仓，占满 1 个名额
		intent("a", 10, 10000, 0.9), // 同码加仓，不占名额，额度 = 4 万 − 3 万 = 1 万
	}, batchState(100000, 100000, 0, 0))
	if !batch[0].Approved || !batch[1].Approved {
		t.Fatalf("新仓 + 同码加仓都应通过: %+v %+v", batch[0], batch[1])
	}
	if batch[1].Amount != model.FromFloat(10000) {
		t.Errorf("同码加仓金额=%s, 期望 10000.00", batch[1].Amount)
	}
}

// TestBuysBlockedRejectsEveryIntent 弱势试探期买入闸（20261103 定参①）：
// BuysBlocked 优先于一切买入判定，新仓与加仓一律拦，且 reject_rule 必须是
// weak_buy_disabled —— 组长要求轨迹里能分清"弱势不许买"与"名额/行业上限"，
// 事后归因才能区分"拦对了"和"拦错了"。
func TestBuysBlockedRejectsEveryIntent(t *testing.T) {
	p := WeakParams(g1Params(12000), 0.20, 0.10)
	if !p.BuysBlocked {
		t.Fatalf("弱势档默认应置 BuysBlocked（signal.weak_buy_enabled 默认 false）")
	}
	got := NewManager(p).CheckBatch([]BuyIntent{
		{TsCode: "a", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(1000)},
		{TsCode: "b", Industry: "银行", RefPrice: model.FromFloat(8), Confidence: 0.9, WantFen: model.FromFloat(800)},
	}, batchState(12000, 11000, 1000, 1, "a"))
	for _, d := range got {
		if d.Approved || d.RejectRule != RuleBuysBlocked {
			t.Errorf("%s 应被 weak_buy_disabled 拦，实际 %+v", d.TsCode, d)
		}
		if !strings.Contains(d.RejectMsg, "弱势") || !strings.Contains(d.RejectMsg, "weak_buy_enabled") {
			t.Errorf("拒绝理由应写明政策出处与回退键，实际 %q", d.RejectMsg)
		}
	}
}

// TestFrictionGateBlocksDefaultParams 摩擦闸在"无显著归因证据"（expected_excess_pct=0）
// 时必须拦掉本轮实盘那种小额单：这是组长确认的设计意图（宁可长期空仓等样本变强），
// 不是缺陷。拒绝理由必须带上摩擦率与阈值两个数，否则事后无法复核拦得对不对。
//
// 这里刻意把单笔下限放开（MinAmountRatio 调小）来隔离被测规则：实盘上 821 元这一档
// 在弱势档还会先撞上 min_amount（0.75×单票上限≈880 元），两条闸都拦是对的，
// 但断言 reject_rule 时必须知道是哪一条先说话。
func TestFrictionGateBlocksDefaultParams(t *testing.T) {
	p := DefaultParams(model.FromFloat(11727.74)) // 空仓后的真实总资产
	if p.ExpectedExcessPct != 0 {
		t.Fatalf("DefaultParams.ExpectedExcessPct=%v，期望 0（无显著证据即全拦）", p.ExpectedExcessPct)
	}
	p.MinAmountRatio = 0.01
	got := NewManager(p).CheckBatch([]BuyIntent{
		{TsCode: "601328.SH", Industry: "银行", RefPrice: model.FromFloat(8.21),
			Confidence: 0.82, WantFen: model.FromFloat(1100)},
	}, batchState(11727.74, 11727.74, 0, 0))
	if got[0].Approved || got[0].RejectRule != RuleFrictionGate {
		t.Fatalf("默认参数下应被摩擦闸拦掉，实际 %+v", got[0])
	}
	if !strings.Contains(got[0].RejectMsg, "≥") || !strings.Contains(got[0].RejectMsg, "expected_excess_pct") {
		t.Errorf("拒绝理由应写明摩擦/阈值与回退键，实际 %q", got[0].RejectMsg)
	}
}

// TestFrictionGateAllowsWhenExcessJustified 摩擦闸只拦"确定成本盖过预期收益"的单子：
// 阈值给到显著正值后，费率低的单必须放行，否则这道闸等于永久停买。
// 同时钉住"摩擦率与名义额反比"：同一阈值下 595 元级被拦、5,000 元级放行。
func TestFrictionGateAllowsWhenExcessJustified(t *testing.T) {
	p := DefaultParams(model.FromFloat(50000))
	p.ExpectedExcessPct = 0.005 // 0.5% 的预期持有期超额
	p.MinAmountRatio = 0.01     // 放开单笔下限，本用例只测摩擦闸
	small := NewManager(p).CheckBatch([]BuyIntent{
		{TsCode: "s", RefPrice: model.FromFloat(5.95), Confidence: 0.9, WantFen: model.FromFloat(600)},
	}, batchState(50000, 50000, 0, 0))
	if small[0].Approved || small[0].RejectRule != RuleFrictionGate {
		t.Errorf("595 元单摩擦约 1.79%% > 阈值 0.5%%，应被拦，实际 %+v", small[0])
	}
	big := NewManager(p).CheckBatch([]BuyIntent{
		{TsCode: "b", RefPrice: model.FromFloat(10), Confidence: 0.9, WantFen: model.FromFloat(5000)},
	}, batchState(50000, 50000, 0, 0))
	if !big[0].Approved {
		t.Errorf("5,000 元单摩擦约 0.30%% < 阈值 0.5%%，应放行，实际 %+v", big[0])
	}
}
