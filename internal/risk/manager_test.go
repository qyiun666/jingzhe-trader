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
func g1Params(totalAssetYuan float64) RiskParams {
	return DefaultParams(model.Fen(int64(totalAssetYuan * 100)))
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
