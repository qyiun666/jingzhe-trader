package scheduler

import (
	"testing"

	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/signal"
)

// TestDesignGateRules 是 #17 的判据：只有全部否决都出自设计内运行期开关时，
// "有批准却 0 买单"才允许从 urgent/M6 降成 partial 降级。
//
// 11-03 那次是 weak_buy_enabled=false 拒掉唯一批准的买单，照样打了 ALL_REJECTED urgent
// 并发 M6。反过来若把真故障（额度、现金、一手买不起、非法价格）也降下来，就是把
// 唯一能喊人的通道自己堵死——所以"混进一条非开关否决"必须是 false。
func TestDesignGateRules(t *testing.T) {
	cases := []struct {
		name    string
		rules   []string
		wantOK  bool
		wantHit []string
	}{
		{name: "空清单不判为设计内", rules: nil, wantOK: false},
		{name: "弱势买入闸", rules: []string{risk.RuleBuysBlocked}, wantOK: true,
			wantHit: []string{risk.RuleBuysBlocked}},
		{name: "摩擦闸", rules: []string{risk.RuleFrictionGate}, wantOK: true,
			wantHit: []string{risk.RuleFrictionGate}},
		{name: "两道开关混合按固定顺序输出",
			rules:  []string{risk.RuleFrictionGate, risk.RuleBuysBlocked, risk.RuleFrictionGate},
			wantOK: true, wantHit: []string{risk.RuleBuysBlocked, risk.RuleFrictionGate}},
		{name: "混进账户结构否决必须仍算故障",
			rules: []string{risk.RuleBuysBlocked, risk.RuleMaxPositions}, wantOK: false},
		{name: "单票额度否决不是政策", rules: []string{risk.RuleSinglePositionFull}, wantOK: false},
		{name: "规则名为空不认", rules: []string{""}, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := make([]signal.Rejection, 0, len(tc.rules))
			for _, r := range tc.rules {
				rs = append(rs, signal.Rejection{TsCode: "600000.SH", Rule: r, Msg: "test"})
			}
			hit, ok := designGateRules(rs)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, 期望 %v（hit=%v）", ok, tc.wantOK, hit)
			}
			if !tc.wantOK {
				if hit != nil {
					t.Errorf("判为故障时不该回显规则名，实际 %v", hit)
				}
				return
			}
			if len(hit) != len(tc.wantHit) {
				t.Fatalf("规则名数 %v, 期望 %v", hit, tc.wantHit)
			}
			for i := range hit {
				if hit[i] != tc.wantHit[i] {
					t.Fatalf("规则名顺序 %v, 期望 %v（降级文案要可复现）", hit, tc.wantHit)
				}
			}
		})
	}
}
