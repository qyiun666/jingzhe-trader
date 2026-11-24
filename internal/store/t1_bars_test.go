package store

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestT1BarsForReadsFourRealPriceColumns T1BarsFor 是 T+1 回收的唯一取数口（组长 20261124
// 裁定 3 的 (b′)：不动已复验的 LimitRowsFor，另开一条按代码集过滤的最小只读）。
// 锁三件事：①只按 codes+date 取四列真实价（Close/VolLot 刻意不取，形状里没有就不会误读）；
// ②缺失键 map（当日没出线 ⇒ 键不出现，而不是回一个四列全 0 的行冒充"0 元"）；
// ③空代码集不查库、不报错（回收首日没有上一交易日快照是正常状态，不是故障）。
func TestT1BarsForReadsFourRealPriceColumns(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	rc := s.MarketRepo()

	const d = "20261124"
	fen := func(v model.Fen) *model.Fen { return &v }
	// 600000.SH：四列齐。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: d, Close: 1050, RawClose: 1000}); err != nil {
		t.Fatal(err)
	}
	hit, err := rc.UpdateBarQuote(ctx, "600000.SH", d, BarQuote{
		Open: fen(980), High: fen(1060), Low: fen(970),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("UpdateBarQuote 未命中既有行，夹具没建起来")
	}
	// 600001.SH：只有收盘，open/high/low 是库里 NULL ⇒ 必须是 0（回收侧按"形态不判"处理）。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600001.SH", TradeDate: d, Close: 900, RawClose: 900}); err != nil {
		t.Fatal(err)
	}
	// 邻日行 + 未请求的代码：都不得混进结果。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: "20261123", Close: 1040, RawClose: 990}); err != nil {
		t.Fatal(err)
	}
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600002.SH", TradeDate: d, Close: 800, RawClose: 800}); err != nil {
		t.Fatal(err)
	}

	m, err := rc.T1BarsFor(ctx, d, []string{"600000.SH", "600001.SH", "600009.SH"})
	if err != nil {
		t.Fatalf("T1BarsFor 失败: %v", err)
	}
	if len(m) != 2 {
		t.Fatalf("键数应为 2（600009.SH 库里无行）: %v", m)
	}
	if _, ok := m["600009.SH"]; ok {
		t.Errorf("缺失键语义被破坏：无行的代码出现在了 map 里")
	}
	if _, ok := m["600002.SH"]; ok {
		t.Errorf("未请求的代码混进了结果")
	}
	b := m["600000.SH"]
	if b.Open != 980 || b.High != 1060 || b.Low != 970 || b.RawClose != 1000 {
		t.Errorf("四列取值应来自真实价列，实得 %+v", b)
	}
	if b2 := m["600001.SH"]; b2.Open != 0 || b2.High != 0 || b2.Low != 0 || b2.RawClose != 900 {
		t.Errorf("NULL 列应读成 0（unknown）而 raw_close 照读，实得 %+v", b2)
	}

	empty, err := rc.T1BarsFor(ctx, d, nil)
	if err != nil {
		t.Fatalf("空代码集不得报错: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空代码集应返回空 map，实得 %v", empty)
	}
}
