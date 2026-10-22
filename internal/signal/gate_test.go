package signal

import (
	"context"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// TestIndexStateGateDisabled 门槛开关决定卖出规则 5 的数据面：
// 关闭时不读指数（零值 ⇒ bad()=false ⇒ evalMarketBad 不触发），指数数据问题
// 不再有能力把决策链拦停；开启时缺数据必须报错——拿不到基准就把"大盘恶化"
// 判成"没恶化"，等于这条规则今天没跑却没人知道。
func TestIndexStateGateDisabled(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/gate.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	off := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: false, Window: 60}, nil, RankOutPolicy{})
	info, err := off.indexState(ctx, "20260924")
	if err != nil {
		t.Fatalf("门槛关闭时 indexState 不应依赖指数数据: %v", err)
	}
	if info.bad() {
		t.Errorf("门槛关闭时不得判出大盘恶化，实际 %+v", info)
	}

	on := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: true, Window: 60}, nil, RankOutPolicy{})
	if _, err := on.indexState(ctx, "20260924"); err == nil {
		t.Errorf("门槛开启且指数无数据时应报错（静默放行才是危险），实际返回 nil")
	}

	// 弱势减仓启用后，gate=false 也要读指数（否则规则 6 永远没有输入）：
	// 读不到只记 WEAK_EXIT_DATA_MISSING、不阻断止损/移动止盈/止盈。
	offWeak := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: false, Window: 60}, nil,
		RankOutPolicy{}).WithWeakExit(WeakExitPolicy{Enabled: true, DropPct: 0.03, ReducePct: 0.5})
	info, err = offWeak.indexState(ctx, "20260924")
	if err != nil {
		t.Fatalf("弱势减仓缺指数数据不应阻断卖出链: %v", err)
	}
	if !strings.Contains(info.weakExitNote, "WEAK_EXIT_DATA_MISSING") {
		t.Errorf("缺数据必须留 WEAK_EXIT_DATA_MISSING，实际 %+v", info)
	}
}

// TestIndexStateWeakExitReadsDrop gate=false + 弱势减仓启用：读出指数收盘/均线/5 日跌幅；
// gate=true 时不读 5 日跌幅（规则 5 接管，规则 6 不参与）。
func TestIndexStateWeakExitReadsDrop(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/gateweak.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	days := []string{"20261009", "20261012", "20261013", "20261014", "20261015", "20261016"}
	closes := []model.Fen{10000, 9900, 9800, 9700, 9600, 9500}
	for i, d := range days {
		if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
			TsCode: store.MarketIndex, TradeDate: d, Close: closes[i]}); err != nil {
			t.Fatalf("写指数日线失败: %v", err)
		}
	}
	policy := WeakExitPolicy{Enabled: true, DropPct: 0.03, ReducePct: 0.5}

	off := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: false, Window: 3}, nil,
		RankOutPolicy{}).WithWeakExit(policy)
	info, err := off.indexState(ctx, "20261016")
	if err != nil {
		t.Fatalf("gate=false 读指数失败: %v", err)
	}
	if !info.bad() || info.weakExitNote != "" {
		t.Fatalf("应读出指数跌破均线且无降级说明，实际 %+v", info)
	}
	if diff := info.ret5 - (-0.05); diff > 1e-9 || diff < -1e-9 {
		t.Errorf("5 日涨跌幅=%.6f，期望 -0.05", info.ret5)
	}

	on := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: true, Window: 3}, nil,
		RankOutPolicy{}).WithWeakExit(policy)
	info, err = on.indexState(ctx, "20261016")
	if err != nil {
		t.Fatalf("gate=true 读指数失败: %v", err)
	}
	if info.ret5 != 0 {
		t.Errorf("gate=true 不应读取弱势减仓的 5 日跌幅，实际 %.6f", info.ret5)
	}
}
