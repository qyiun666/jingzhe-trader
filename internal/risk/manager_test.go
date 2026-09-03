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

// g1Params 总资产 10 万元的 G1 生效参数（单票 4 万 / 总仓 9 万 / 最大持仓自适应 6）。
func g1Params(totalAssetYuan float64) RiskParams {
	return Resolve(DefaultBase(model.Fen(int64(totalAssetYuan*100))), model.GearG1, false, NoPace{})
}

// TestNoBatchOverTotalPositionLimit 对应验收 #7（历史 P0 bug 回归）：
// 同一批 3 笔买入，单笔均不超单票上限（4 万），但合计 12 万超总仓位上限（9 万）
// → 第 3 笔必须被拒，且 reject_rule == max_total_position。
func TestNoBatchOverTotalPositionLimit(t *testing.T) {
	m := NewManager(g1Params(100000))
	intents := []BuyIntent{
		{TsCode: "sh600001", Name: "甲", RefPrice: model.FromFloat(10.0), Confidence: 0.9, Score: 80},
		{TsCode: "sh600002", Name: "乙", RefPrice: model.FromFloat(10.0), Confidence: 0.9, Score: 80},
		{TsCode: "sh600003", Name: "丙", RefPrice: model.FromFloat(10.0), Confidence: 0.9, Score: 80},
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
	// 前两笔金额均为单票上限 4 万（总仓 9 万未破）
	if got[0].Amount != model.FromFloat(40000) {
		t.Errorf("首笔金额=%s, 期望 40000.00", got[0].Amount)
	}
}

// TestBatchRejectionWithExistingPositions 已有持仓市值计入累计基数：2 万持仓 + 3×4 万同样在第 3 笔被拒。
func TestBatchRejectionWithExistingPositions(t *testing.T) {
	m := NewManager(g1Params(100000)) // 总仓上限 9 万
	intents := []BuyIntent{
		{TsCode: "sh600001", RefPrice: model.FromFloat(10.0), Confidence: 0.9, Score: 80},
		{TsCode: "sh600002", RefPrice: model.FromFloat(10.0), Confidence: 0.9, Score: 80},
		{TsCode: "sh600003", RefPrice: model.FromFloat(10.0), Confidence: 0.9, Score: 80},
	}
	// 现有持仓 1 万（现金 9 万）：1 万+4 万=5 万、+4 万=9 万恰好触顶，第 3 笔爆掉
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
	p := g1Params(100000) // 单票 4 万 / 总仓 9 万 / 持仓上限 6 / 置信度 0.55 / 最小 5000 元
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
			intents:  []BuyIntent{{TsCode: "a", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80}},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleAllowNewPosition, wantIndex: 0,
		},
		{
			name: "置信度不足", params: p,
			intents:  []BuyIntent{{TsCode: "a", RefPrice: model.FromFloat(10), Confidence: 0.50, Score: 80}},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleMinConfidence, wantIndex: 0,
		},
		{
			name: "评分低于门槛", params: withMul(p, 1.5),
			intents:  []BuyIntent{{TsCode: "a", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80}},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleScoreThreshold, wantIndex: 0,
		},
		{
			name: "重复持仓", params: p,
			intents:  []BuyIntent{{TsCode: "a", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80}},
			state:    batchState(100000, 100000, 0, 1, "a"),
			wantRule: RuleAlreadyHolding, wantIndex: 0,
		},
		{
			name: "持仓数达上限", params: withMaxPos(p, 1),
			intents:  []BuyIntent{{TsCode: "b", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80}},
			state:    batchState(100000, 100000, 0, 1, "a"),
			wantRule: RuleMaxPositions, wantIndex: 0,
		},
		{
			name: "单笔金额低于下限", params: withMinAmount(p, model.FromFloat(60000)),
			intents:  []BuyIntent{{TsCode: "a", RefPrice: model.FromFloat(100), Confidence: 0.9, Score: 80}},
			state:    batchState(100000, 100000, 0, 0),
			wantRule: RuleMinAmount, wantIndex: 0,
		},
		{
			name: "现金不足", params: p,
			intents:  []BuyIntent{{TsCode: "a", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80}},
			state:    batchState(100000, 1000, 0, 0),
			wantRule: RuleMinAmount, wantIndex: 0, // 1000 元现金折算后不足 5000 元下限
		},
		{
			name: "参考价非法", params: p,
			intents:  []BuyIntent{{TsCode: "a", RefPrice: 0, Confidence: 0.9, Score: 80}},
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
		{TsCode: "a", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80},
		{TsCode: "b", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80},
		{TsCode: "c", RefPrice: model.FromFloat(10), Confidence: 0.9, Score: 80},
	}
	got := m.CheckBatch(intents, batchState(100000, 100000, 0, 0))
	if !got[0].Approved || !got[1].Approved {
		t.Errorf("前两笔应通过: %+v %+v", got[0], got[1])
	}
	if got[2].Approved || got[2].RejectRule != RuleMaxPositions {
		t.Errorf("第 3 笔应因 max_positions 被拒: %+v", got[2])
	}
}

// TestPlanBuy 金额→股数核算：整手向下取整 + 最小金额校验。
func TestPlanBuy(t *testing.T) {
	p := g1Params(100000) // 单票上限 4 万，最小 5000 元
	qty, amount, err := PlanBuy(p, model.FromFloat(100000), model.FromFloat(37.8))
	if err != nil {
		t.Fatalf("PlanBuy 失败: %v", err)
	}
	if qty != 1000 {
		t.Errorf("qty=%d, 期望 1000（37800/37.8≈1000 手取整）", qty)
	}
	if amount != model.FromFloat(37800) {
		t.Errorf("amount=%s, 期望 37800.00", amount)
	}
	// 低价股不超过单票上限
	qty2, _, err2 := PlanBuy(p, model.FromFloat(100000), model.FromFloat(2.5))
	if err2 != nil {
		t.Fatalf("PlanBuy(低价股) 失败: %v", err2)
	}
	if qty2 != 16000 {
		t.Errorf("低价股 qty=%d, 期望 16000（40000/2.5 整手）", qty2)
	}
	// 高价股低于最小交易额 → 报错
	if _, _, err3 := PlanBuy(p, model.FromFloat(100000), model.FromFloat(800)); err3 == nil {
		t.Errorf("800 元/股 × 整手远超现金，应返回错误")
	}
}

// TestTargetQty 整手取整边界。
func TestTargetQty(t *testing.T) {
	cases := []struct {
		amount, price model.Fen
		want          model.Qty
	}{
		{model.FromFloat(10000), model.FromFloat(10), 1000},
		{model.FromFloat(10050), model.FromFloat(10), 1000},   // 向下取整
		{model.FromFloat(99), model.FromFloat(10), 0},         // 不足一手
		{model.FromFloat(10000), 0, 0},                        // 非法价格
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

func withMul(p RiskParams, mul float64) RiskParams {
	p.ScoreThresholdMul = mul
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
