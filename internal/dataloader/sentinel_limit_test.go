package dataloader

import (
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/tushare"
)

// TestSentinelUpLimitValuesBecomeUnknown 钉住组长 20261123 裁定二·方案 (a) 的上限侧面：
// stk_limit 表达「本品种当日无价格涨跌幅限制」用的是**极值哨兵**而不是 NULL/0，必须在
// 写入侧归一为 unknown(NULL)，消费侧不许各自复制阈值。
//
// 表里的三个哨兵值与 20261123 回填批的实测一一对应（上限面共 26 行 / 12 只）：
//
//	9999999    北交所新股 920xxx（6 行，同批 down_limit 给 0）
//	10000000   科创板 688827.SH（5 行，同批 down_limit 给 1）
//	100000000  创业板/主板新股 301xxx、001381、002731（15 行，同批 down_limit 给 1）
//
// 裁定给的是**精确值集合**，不是阈值：本用例同时钉住"相邻量级不得被顺手扩大"——
// 3,300,000（3.3 万元，对应千元股涨停）必须照落，否则清洗会吃掉真值。
func TestSentinelUpLimitValuesBecomeUnknown(t *testing.T) {
	cases := []struct {
		in        int64
		wantValue bool
		why       string
	}{
		{9999999, false, "北交所新股无上限哨兵（920067.BJ 20261123 实测）"},
		{10000000, false, "科创板新股无上限哨兵（688827.SH 20261110 实测）"},
		{100000000, false, "创业板/主板新股无上限哨兵（301569.SZ 20261019 实测）"},
		{0, false, "接口给 0 价，既有 unknown 行为不变"},
		{-5, false, "异常负值同样不落库"},
		{1155, true, "600000.SH 真实涨停 10.55 元"},
		{3300000, true, "3.3 万元量级（千元股涨停）不是哨兵，必须照落"},
		{9999998, true, "紧邻哨兵下方一位：精确集合不做阈值扩大"},
		{10000001, true, "紧邻哨兵上方一位：同上"},
	}
	for _, c := range cases {
		got := upLimitFen(model.Fen(c.in))
		if (got != nil) != c.wantValue {
			t.Errorf("upLimitFen(%d) 得 %v，期望非nil=%v（%s）", c.in, got, c.wantValue, c.why)
		}
		if got != nil && *got != model.Fen(c.in) {
			t.Errorf("upLimitFen(%d) 改写了原值：%d", c.in, int64(*got))
		}
	}
}

// TestSentinelDownLimitValuesBecomeUnknown 下限侧面：实测 20 行 `down_limit=1`
// （0.01 元）与上面的极值上限同批出现，是"没有下限"的编码。真实跌停价到不了这一量级
// （面值退市边缘的个股也在 10 分以上），所以按裁定的精确判据 `==1` 不会吃掉真值。
func TestSentinelDownLimitValuesBecomeUnknown(t *testing.T) {
	cases := []struct {
		in        int64
		wantValue bool
		why       string
	}{
		{1, false, "0.01 元下限哨兵，与无上限极值同批（实测 20 行）"},
		{0, false, "北交所新股那 6 行的下限就是 0，既有行为归 unknown"},
		{-1, false, "异常负值"},
		{2, true, "紧邻哨兵上方一位：精确判据不做阈值扩大"},
		{12, true, "0.12 元（面值退市边缘个股的真实跌停档）必须照落"},
		{945, true, "600000.SH 真实跌停 9.45 元"},
	}
	for _, c := range cases {
		got := downLimitFen(model.Fen(c.in))
		if (got != nil) != c.wantValue {
			t.Errorf("downLimitFen(%d) 得 %v，期望非nil=%v（%s）", c.in, got, c.wantValue, c.why)
		}
		if got != nil && *got != model.Fen(c.in) {
			t.Errorf("downLimitFen(%d) 改写了原值：%d", c.in, int64(*got))
		}
	}
}

// TestBarQuoteOfSentinelPairKeepsOHLC 验证清洗只作用于两列限价：同一行里 OHLC 来自
// daily 的真实开高低，与限价的哨兵无关，不得被牵连成 unknown —— 否则一次新股行情会把
// 当日四列价格一起洗掉。
func TestBarQuoteOfSentinelPairKeepsOHLC(t *testing.T) {
	b := model.Bar{TsCode: "301569.SZ", TradeDate: "20261019", Open: 2300, High: 2590, Low: 2200}

	// 哨兵对（新股无涨跌幅限制）：两列限价 nil，三列 OHLC 照旧有值。
	q := barQuoteOf(b, tushare.StkLimitRow{UpLimit: model.Fen(100000000), DownLimit: model.Fen(1)}, true)
	if q.UpLimit != nil || q.DownLimit != nil {
		t.Fatalf("哨兵限价应归 unknown，实得 up=%v down=%v", q.UpLimit, q.DownLimit)
	}
	if q.Open == nil || *q.Open != 2300 || q.High == nil || *q.High != 2590 || q.Low == nil || *q.Low != 2200 {
		t.Fatalf("OHLC 不该被限价清洗牵连：%+v", q)
	}

	// 正常对：原样落值，基准仍是未复权真实价（不乘复权因子）。
	q2 := barQuoteOf(b, tushare.StkLimitRow{UpLimit: model.Fen(2530), DownLimit: model.Fen(1870)}, true)
	if q2.UpLimit == nil || *q2.UpLimit != 2530 || q2.DownLimit == nil || *q2.DownLimit != 1870 {
		t.Fatalf("正常限价应原样落值，实得 %+v", q2)
	}

	// stk_limit 未返回该代码：两列 nil（limit_unknown 面），OHLC 照旧。
	q3 := barQuoteOf(b, tushare.StkLimitRow{}, false)
	if q3.UpLimit != nil || q3.DownLimit != nil {
		t.Fatalf("无限价行应两列 unknown，实得 %+v", q3)
	}
	if q3.Open == nil || q3.High == nil || q3.Low == nil {
		t.Fatalf("无限价行仍应有 OHLC：%+v", q3)
	}
}
