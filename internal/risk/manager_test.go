package risk

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// batchState 构造批次核算的账户状态。
func batchState(totalAssetYuan, cashYuan, positionsMVYuan float64, posCount int, held ...string) AccountState {
	heldSet := make(map[string]bool, len(held))
	for _, c := range held {
		heldSet[c] = true
	}
	return AccountState{
		TotalAsset:    model.Fen(totalAssetYuan * 100),
		Cash:          model.Fen(cashYuan * 100),
		PositionsMV:   model.Fen(positionsMVYuan * 100),
		PositionCount: posCount,
		HeldCodes:     heldSet,
	}
}

// g1Params 按总资产解析出的 G1 生效参数。10 万元：单票 4 万 / 总仓 9 万 / 最大持仓 6 / 置信度 0.55。
func g1Params(totalAssetYuan float64) RiskParams {
	return mustResolve(DefaultBase(model.Fen(int64(totalAssetYuan*100))), model.GearG1, false, NoPace{})
}

// mustResolve Resolve 的测试包装：用例传的都是表内合法档位，返回错误说明测试代码本身写错。
func mustResolve(base RiskParams, gear model.Gear, lock bool, pace PaceAdjust) RiskParams {
	p, err := Resolve(base, gear, lock, pace)
	if err != nil {
		panic(err)
	}
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
			name: "禁开新仓", params: withBias(p, BiasExitOnly, false),
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
			name: "重复持仓", params: p,
			intents:  []BuyIntent{intent("a", 10, 40000, 0.9)},
			state:    batchState(100000, 100000, 0, 1, "a"),
			wantRule: RuleAlreadyHolding, wantIndex: 0,
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

func withBias(p RiskParams, b StrategyBias, allow bool) RiskParams {
	p.Bias = b
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
