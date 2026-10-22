package risk

import (
	"testing"

	"jingzhe-trader/internal/model"
)

// TestTradeFees 双边费用口径：佣金每笔最低 5 元，卖出加印花税千 1，
// 过户费双边十万分之一。小仓位的成本占比远高于名义费率，这是弱势档
// 提高最小交易额的依据，数值必须钉死。
func TestTradeFees(t *testing.T) {
	cases := []struct {
		yuan             float64
		buy, sell, round model.Fen
	}{
		{5000, 505, 1005, 1510}, // 5.05 / 10.05 / 15.10 元
		{1200, 501, 621, 1122},  // 5.01 / 6.21 / 11.22 元
	}
	for _, tc := range cases {
		amount := model.FromFloat(tc.yuan)
		if got := BuyFeeFen(amount); got != tc.buy {
			t.Errorf("%.0f 元买入费=%s，期望 %s", tc.yuan, got, tc.buy)
		}
		if got := SellFeeFen(amount); got != tc.sell {
			t.Errorf("%.0f 元卖出费=%s，期望 %s", tc.yuan, got, tc.sell)
		}
		if got := RoundTripCostFen(amount); got != tc.round {
			t.Errorf("%.0f 元双边费=%s，期望 %s", tc.yuan, got, tc.round)
		}
	}
}

// TestTradeFeesNonPositive 非正金额不产生费用（调用方已在上游否决，这里只防溢出/负费用）。
func TestTradeFeesNonPositive(t *testing.T) {
	for _, amount := range []model.Fen{0, -1} {
		if got := BuyFeeFen(amount); got != 0 {
			t.Errorf("买入费(%d)=%s，期望 0", amount, got)
		}
		if got := SellFeeFen(amount); got != 0 {
			t.Errorf("卖出费(%d)=%s，期望 0", amount, got)
		}
		if got := RoundTripCostFen(amount); got != 0 {
			t.Errorf("双边费(%d)=%s，期望 0", amount, got)
		}
	}
}
