package scheduler

import (
	"context"
	"testing"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// newWeakFixture 一只股票都没有、现金 11833.80 元的干净账户 + 三日递减的指数收盘。
//
// 递减序列（300/200/100）让末日收盘 100 低于 MA2=150：门槛关闭时判弱势、门槛开启时
// 关闸，两种 regime 都能在同一份数据上跑出来。
func newWeakFixture(t *testing.T) (*store.Store, *ticket.Ledger, context.Context) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/cap.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	days := []string{"20260301", "20260302", "20260303"}
	closes := []model.Fen{300, 200, 100}
	for i, d := range days {
		if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
			TsCode: store.MarketIndex, TradeDate: d, Close: closes[i]}); err != nil {
			t.Fatalf("插入指数日线失败: %v", err)
		}
	}
	// 本金即现金（无持仓、无成交）：11833.80 元 = 实盘账户口径。
	led := ticket.NewLedger(st, market.CostParams{}, model.Fen(1183380))
	return st, led, ctx
}

// rpOf 与 app.RiskParamsOf 同口径：风控参数由账户总资产纯函数推出。
func rpOf(ctx context.Context, led *ticket.Ledger, date string) (risk.RiskParams, error) {
	ast, err := led.Assets(ctx, date)
	if err != nil {
		return risk.RiskParams{}, err
	}
	return risk.DefaultParams(ast.TotalAsset), nil
}

// TestScreenBudgetWeakRegimePerSlotEqualsRiskCap 弱势试探下，漏斗"一手买得起吗"这条线
// 必须等于风控的单票上限，而不是现金均分。
//
// P0 修复的回归锚点（组长 20261009 批准）：修复前 ScreenBudget 不看弱势，漏斗按
// 11833.80/2=5916.90 元放行、决策链按 10%×总资产=1183.38 元砍单，两侧差 5 倍；
// 实跑后果是 20260930 有 7/20、20261008 有 4/20 候选属于"选出来就永远买不起"。
// 这里断言三件事：弱势判定生效、rp 已被收缩、预算线与收缩后的 rp 上限逐分相等。
func TestScreenBudgetWeakRegimePerSlotEqualsRiskCap(t *testing.T) {
	st, led, ctx := newWeakFixture(t)
	date := "20260303"
	rp, err := rpOf(ctx, led, date)
	if err != nil {
		t.Fatalf("生成风控参数失败: %v", err)
	}
	before := rp.SingleCapFen() // 正常档：40%×总资产
	gate := store.MarketGate{Enabled: false, Window: 2}
	b, effRp, err := ScreenBudget(ctx, st, led, date, rp, gate, WeakCaps{
		TotalPct: risk.WeakMaxTotalPctDefault, SinglePct: risk.WeakMaxSinglePctDefault})
	if err != nil {
		t.Fatalf("ScreenBudget 失败: %v", err)
	}
	if !b.WeakRegime {
		t.Fatalf("门槛关闭且收盘在 MA2 下方，应判弱势试探")
	}
	if b.Slots != effRp.MaxPositions || effRp.MaxPositions <= 0 {
		t.Errorf("预算槽位应与生效持仓数一致，实际 slots=%d maxPos=%d", b.Slots, effRp.MaxPositions)
	}
	if b.CapFen != effRp.SingleCapFen() {
		t.Errorf("漏斗预算上限应等于生效风控单票上限，实际 cap=%d 分 rp.SingleCapFen=%d 分",
			b.CapFen, effRp.SingleCapFen())
	}
	want := model.Fen(118338) // 1183.38 元 = 10% × 11833.80
	if b.CapFen != want {
		t.Errorf("弱势上限应为 %d 分（1183.38 元），实际 %d 分", want, b.CapFen)
	}
	if b.CapFen >= before {
		t.Errorf("弱势收缩必须比正常档更严：收缩前 %d 分，收缩后 %d 分", before, b.CapFen)
	}
	// 均分口径必须仍比上限宽，否则这条回归测不到"取严"这件事。
	if avg := b.Cash / model.Fen(b.Slots); b.CapFen >= avg {
		t.Errorf("前提不成立：均分 %.2f 元应严于单票上限 %.2f 元", avg.Float(), b.CapFen.Float())
	}
	// 漏斗与决策链读的是同一份 rp：把返回的 rp 交给 buildTickets 后两侧不可能再分叉。
	if effRp.MaxPositionPct != risk.WeakMaxSinglePctDefault {
		t.Errorf("返回的 rp 应已收缩到试探档，实际单票比例 %.2f", effRp.MaxPositionPct)
	}
	if b.WeakNote == "" {
		t.Errorf("弱势判定必须带可归因的 note")
	}
}

// TestScreenBudgetNormalRegimeUsesRiskCapToo 门槛开启（非弱势）时预算线同样收口到
// 单票上限（40%×总资产），而不是停在现金均分：一致性不分区制。
func TestScreenBudgetNormalRegimeUsesRiskCapToo(t *testing.T) {
	st, led, ctx := newWeakFixture(t)
	date := "20260301" // 末日收盘 300 = MA1，门槛开启时不关闸也不判弱势
	rp, err := rpOf(ctx, led, date)
	if err != nil {
		t.Fatalf("生成风控参数失败: %v", err)
	}
	gate := store.MarketGate{Enabled: true, Window: 1}
	b, effRp, err := ScreenBudget(ctx, st, led, date, rp, gate, WeakCaps{
		TotalPct: risk.WeakMaxTotalPctDefault, SinglePct: risk.WeakMaxSinglePctDefault})
	if err != nil {
		t.Fatalf("ScreenBudget 失败: %v", err)
	}
	if b.WeakRegime {
		t.Errorf("门槛开启时不该判弱势试探（弱势表达方式是关闸）")
	}
	if !b.MarketOK {
		t.Errorf("收盘=MA1 应放行买入漏斗")
	}
	if b.CapFen != effRp.SingleCapFen() || b.CapFen != rp.SingleCapFen() {
		t.Errorf("正常档预算线应等于单票上限：cap=%d rp=%d", b.CapFen, rp.SingleCapFen())
	}
	if want := model.Fen(473352); b.CapFen != want { // 40% × 11833.80
		t.Errorf("正常档上限应为 %d 分（4733.52 元），实际 %d 分", want, b.CapFen)
	}
	// 生效 rp 与传入 rp 在正常档必须是同一个对象口径（未被弱势改写）。
	if effRp.MaxPositionPct != rp.MaxPositionPct {
		t.Errorf("正常档不应改写单票比例：%v vs %v", effRp.MaxPositionPct, rp.MaxPositionPct)
	}
}

// TestScreenBudgetSlotsFollowMaxPositions 槽位仍按计划持仓数均分：收口只加一条上限线，
// 不把"分几槽"这件事一起改掉（仓位分散度属于 risk.DefaultParams 的自适应口径）。
func TestScreenBudgetSlotsFollowMaxPositions(t *testing.T) {
	st, led, ctx := newWeakFixture(t)
	rp, err := rpOf(ctx, led, "20260303")
	if err != nil {
		t.Fatalf("生成风控参数失败: %v", err)
	}
	b, _, err := ScreenBudget(ctx, st, led, "20260303", rp,
		store.MarketGate{Enabled: false, Window: 2}, WeakCaps{TotalPct: 0.20, SinglePct: 0.10})
	if err != nil {
		t.Fatalf("ScreenBudget 失败: %v", err)
	}
	if b.Slots != rp.MaxPositions {
		t.Errorf("slots 应等于 MaxPositions（1.18 万账户自适应为 2），实际 %d vs %d", b.Slots, rp.MaxPositions)
	}
	var _ screener.Budget = b // 编译期钉住返回类型，改签名会在这里先炸
}
