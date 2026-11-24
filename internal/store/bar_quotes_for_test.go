package store

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestBarQuotesForReadsAllFiveColumns 锁 BarQuotesFor 的四件事，其中第 ④ 条是策略 20261124
// §3 点名的那个陷阱，值得单独写清楚：
//  1. 缺失键 map —— 当日没出线的代码不出现在 map 里（不回一个五列全 nil 的行冒充"都没值"）；
//  2. 按 codes + date 过滤 —— 未请求的代码、邻日的行都不得混进结果；
//  3. 库里 NULL ⇒ 指针 nil（unknown），不是 0；
//  4. **五列必须真的都被 SELECT 过** —— 夹具用 UpdateBarQuote 写进 up_limit/down_limit，
//     再断言它们读回来是非 nil。若哪天有人把 SELECT 改成只取 open/high/low 三列却仍返回
//     BarQuote，这两列会静默变成 nil，而 nil 在本类型里的既有含义是"交易所没供这个档位"：
//     "我没读这一列"与"这一列没有值"共用同一个字面量，回收侧就把有档位的票判成 unknown。
//     这条断言不查库就发现不了，所以它是本用例存在的理由，不是凑数。
func TestBarQuotesForReadsAllFiveColumns(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	rc := s.MarketRepo()

	const d = "20261124"
	fen := func(v model.Fen) *model.Fen { return &v }
	// 600000.SH：五列齐。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: d, Close: 1050, RawClose: 1000}); err != nil {
		t.Fatal(err)
	}
	hit, err := rc.UpdateBarQuote(ctx, "600000.SH", d, BarQuote{
		Open: fen(980), High: fen(1060), Low: fen(970), UpLimit: fen(1050), DownLimit: fen(950),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("UpdateBarQuote 未命中既有行，夹具没建起来")
	}
	// 600001.SH：有收盘、扩展价族五列全是库里 NULL（该接口当日没给）。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600001.SH", TradeDate: d, Close: 900, RawClose: 900}); err != nil {
		t.Fatal(err)
	}
	// 600002.SH：只有限价两列有值、OHLC 三列 NULL——回收要能分别出"档位有、形态没数"。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600002.SH", TradeDate: d, Close: 820, RawClose: 810}); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.UpdateBarQuote(ctx, "600002.SH", d, BarQuote{UpLimit: fen(891), DownLimit: fen(729)}); err != nil {
		t.Fatal(err)
	}
	// 邻日行 + 未请求的代码：都不得混进结果。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: "20261123", Close: 1040, RawClose: 990}); err != nil {
		t.Fatal(err)
	}
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600003.SH", TradeDate: d, Close: 700, RawClose: 700}); err != nil {
		t.Fatal(err)
	}

	m, err := rc.BarQuotesFor(ctx, d, []string{"600000.SH", "600001.SH", "600002.SH", "600009.SH"})
	if err != nil {
		t.Fatalf("BarQuotesFor 失败: %v", err)
	}
	if len(m) != 3 {
		t.Fatalf("键数应为 3（600009.SH 库里无行）: %v", m)
	}
	if _, ok := m["600009.SH"]; ok {
		t.Errorf("缺失键语义被破坏：无行的代码出现在了 map 里")
	}
	if _, ok := m["600003.SH"]; ok {
		t.Errorf("未请求的代码混进了结果")
	}

	b := m["600000.SH"]
	if b.Open == nil || *b.Open != 980 || b.High == nil || *b.High != 1060 || b.Low == nil || *b.Low != 970 {
		t.Errorf("OHLC 三列应取到真实价，实得 %+v", b)
	}
	if b.UpLimit == nil || *b.UpLimit != 1050 || b.DownLimit == nil || *b.DownLimit != 950 {
		t.Errorf("两列限价必须读回非 nil（写成 nil 说明 SELECT 漏了这两列，见用例注释第 ④ 条），实得 up=%v down=%v",
			b.UpLimit, b.DownLimit)
	}
	// raw_close 刻意不在本读法里（BarQuote 无该字段）：回收侧从同日的 LimitRowsFor 取。

	// NULL 五列必须全是 nil，而不是指向 0 的指针（0 在库里是"不存在"的写法，见 schema.go 口径 1）。
	if b1 := m["600001.SH"]; b1.Open != nil || b1.High != nil || b1.Low != nil || b1.UpLimit != nil || b1.DownLimit != nil {
		t.Errorf("库里 NULL 必须读成 nil（unknown），实得 %+v", b1)
	}
	b2 := m["600002.SH"]
	if b2.Open != nil || b2.High != nil || b2.Low != nil {
		t.Errorf("OHLC 缺价应为 nil 而档位有值，实得 %+v", b2)
	}
	if b2.UpLimit == nil || *b2.UpLimit != 891 || b2.DownLimit == nil || *b2.DownLimit != 729 {
		t.Errorf("限价两列应各自独立取到，实得 up=%v down=%v", b2.UpLimit, b2.DownLimit)
	}

	empty, err := rc.BarQuotesFor(ctx, d, nil)
	if err != nil {
		t.Fatalf("空代码集不得报错: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空代码集应返回空 map，实得 %v", empty)
	}
}
