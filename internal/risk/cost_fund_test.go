package risk

import (
	"math"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestFundFeeOmitsStampTax 场内基金档的唯一差别是卖出印花税（20261104 策略 B3）。
// 断言用"差额恰好等于印花税"而不是绝对数值：绝对值变了说明费率常量被人动过，
// 而这里要钉住的是"少一项税"这个结构性事实。
func TestFundFeeOmitsStampTax(t *testing.T) {
	for _, yuan := range []float64{880, 948, 1173.78, 4695, 11737.83} {
		amount := model.FromFloat(yuan)
		stock := RoundTripCostFen(amount)
		fund := RoundTripCostFundFen(amount)
		want := model.Fen(math.Round(float64(amount) * StampTaxRate))
		if stock-fund != want {
			t.Errorf("%.2f 元：个股档 %s − 基金档 %s 应恰好等于卖出印花税 %s",
				yuan, stock, fund, want)
		}
		if fund >= stock {
			t.Errorf("%.2f 元：基金档应比个股档便宜，实际 %s vs %s", yuan, fund, stock)
		}
		// 买入侧两档同值：买入本来就没有印花税。
		if BuyFeeFen(amount) != BuyFeeFen(amount) {
			t.Errorf("买入侧不应出现档位差")
		}
		if got := SellFeeFenFor(amount, true); got != SellFeeFundFen(amount) {
			t.Errorf("SellFeeFenFor 与 SellFeeFundFen 应同值：%s vs %s", got, SellFeeFundFen(amount))
		}
		if got := SellFeeFenFor(amount, false); got != SellFeeFen(amount) {
			t.Errorf("SellFeeFenFor(false) 必须与旧的个股档逐位一致：%s vs %s", got, SellFeeFen(amount))
		}
		if nonPositive := SellFeeFenFor(0, true); nonPositive != 0 {
			t.Errorf("金额非正时卖出费用应为 0，实际 %s", nonPositive)
		}
	}
}

// TestFrictionPctProfiles 摩擦率按档位取值，且非正金额一律按"成本吃光收益"处理。
// 11-03 实测小单双边摩擦 1.09%~1.78%、11-04 晚报里 ETF 级名义额应低于个股档，
// 这条测试把两个档位的关系钉死，防止"ETF 看起来比实际更贵"把它提前否掉。
func TestFrictionPctProfiles(t *testing.T) {
	if f := FrictionPct(0); f != 1 {
		t.Errorf("金额为 0 应按最保守的 1 处理，实际 %v", f)
	}
	if f := FrictionPctFund(-100); f != 1 {
		t.Errorf("非正金额在基金档同样应返回 1，实际 %v", f)
	}
	amount := model.FromFloat(948) // 588200 一手 79 元 × 12 手
	stock, fund := FrictionPct(amount), FrictionPctFund(amount)
	if !(fund < stock) {
		t.Errorf("948 元名义：基金档 %.4f 应低于个股档 %.4f", fund, stock)
	}
	if got := FrictionPctFor(amount, true); got != fund {
		t.Errorf("FrictionPctFor(true) 与 FrictionPctFund 应同值")
	}
	if got := FrictionPctFor(amount, false); got != stock {
		t.Errorf("FrictionPctFor(false) 与 FrictionPct 应同值")
	}
	// 量级复核（11-03 结论的口径）：千元级小单摩擦在 1% 上下，不是合同费率 0.16%。
	if stock < 0.008 || stock > 0.015 {
		t.Errorf("948 元个股档摩擦率应在 0.8%%~1.5%% 区间，实际 %.4f", stock)
	}
}

// TestTrailingArmProfiles 武装线随档位下降：按个股档估 ETF 的摩擦会把武装线抬高，
// 高点的利润因此不能及时锁住——这正是 B3 在 ETF 开闸前必须修的原因。
func TestTrailingArmProfiles(t *testing.T) {
	p := DefaultParams(model.FromFloat(11737.83))
	notional := model.FromFloat(948)
	stock := p.TrailingArmPctFor(notional)
	fund := p.TrailingArmPctForFund(notional)
	if fund > stock {
		t.Errorf("基金档武装线不应更高：%.4f vs %.4f", fund, stock)
	}
	if got := p.TrailingArmPctForVariant(notional, true); got != fund {
		t.Errorf("TrailingArmPctForVariant(true) 与 TrailingArmPctForFund 应同值")
	}
	// 旧口径回退开关仍然有效：TrailingArmPct<=0 时两档都返回 0，不被动态项覆盖。
	off := p
	off.TrailingArmPct = 0
	if off.TrailingArmPctFor(notional) != 0 || off.TrailingArmPctForFund(notional) != 0 {
		t.Errorf("设 0 回滚必须对两个档位都成立，否则回退开关形同虚设")
	}
	// 静态下限之上：小单的武装线必须比 8% 更高才可能净赚。
	if stock < p.TrailingArmPct {
		t.Errorf("948 元小单的推导武装线不应低于静态下限")
	}
}

// TestMinAmountForMatchesRiskFloor 漏斗注入的下界与风控自算的下界必须逐位相等，
// 覆盖三档账户规模 × 常规/弱势两个 regime。B1 把下限传进 screener 而不在那边重抄
// 算式，这条测试就是"两处分叉"的守门人。
func TestMinAmountForMatchesRiskFloor(t *testing.T) {
	for _, yuan := range []float64{3000, 11737.83, 50000, 200000} {
		total := model.FromFloat(yuan)
		p := DefaultParams(total)
		if got, want := MinAmountFor(p.SingleCapFen(), false), p.MinAmountFloor(); got != want {
			t.Errorf("%.2f 元常规档：MinAmountFor %s ≠ MinAmountFloor %s", yuan, got, want)
		}
		pw := WeakParams(DefaultParams(total), 0.20, 0.10)
		if got, want := MinAmountFor(pw.SingleCapFen(), true), pw.MinAmountFloor(); got != want {
			t.Errorf("%.2f 元弱势档：MinAmountFor %s ≠ MinAmountFloor %s", yuan, got, want)
		}
	}
	// 11-03 实跑口径复核：单票上限 1,173.78 → 下界约 880.34（0.75 比例）。
	if got := MinAmountFor(model.Fen(117378), true); got < model.Fen(88000) || got > model.Fen(88100) {
		t.Errorf("弱势档 12000 元级账户的单笔下限应约 880.34 元，实际 %s", got)
	}
	// 没有单票上限口径时不得凭空造一个下界出来收紧。
	if got := MinAmountFor(0, true); got != DefaultMinSingleAmountFen {
		t.Errorf("单票上限缺失时应回落绝对下限（与 MinAmountFloor 同处理），实际 %s", got)
	}
	// 大账户：绝对下限 5000 元仍然生效，不被比例式覆盖。
	if got := MinAmountFor(model.Fen(6000000), false); got != DefaultMinSingleAmountFen {
		t.Errorf("单票上限 6 万元的常规档应保持 5000 元绝对下限，实际 %s", got)
	}
}
