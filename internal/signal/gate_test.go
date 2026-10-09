package signal

import (
	"context"
	"testing"

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

	off := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: false, Window: 60}, nil)
	info, err := off.indexState(ctx, "20260924")
	if err != nil {
		t.Fatalf("门槛关闭时 indexState 不应依赖指数数据: %v", err)
	}
	if info.bad() {
		t.Errorf("门槛关闭时不得判出大盘恶化，实际 %+v", info)
	}

	on := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: true, Window: 60}, nil)
	if _, err := on.indexState(ctx, "20260924"); err == nil {
		t.Errorf("门槛开启且指数无数据时应报错（静默放行才是危险），实际返回 nil")
	}
}
