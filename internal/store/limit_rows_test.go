package store

import (
	"context"
	"sort"
	"testing"

	"jingzhe-trader/internal/model"
)

// limitKeys 排序返回 LimitRowsFor 结果的键集，便于把"缺失键"写成可核对的断言而不是长度。
func limitKeys(m map[string]LimitRow) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestLimitRowsForMissingKeyMap 锁死读法的第一条语义：**库里没有这根线，代码就不出现在
// map 里**。零值的 LimitRow 带着 RawClose=0，读起来像"今天收在 0.00 元"，而事实是
// 停牌/退市/还没同步 —— 两种事实在库里长得一样时，下游一定会把后者当成前者处理。
// 让不存在可判只需要 `v, ok := m[code]`；让零值不可判要在每个消费点补一次 RawClose>0，
// 而后者一定会漏（今晚实测 stk_limit 有行而库无行的量是 2,191 只·日）。
func TestLimitRowsForMissingKeyMap(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	rc := s.MarketRepo()

	const d = "20261123"
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: d, Close: 1050, RawClose: 1050}); err != nil {
		t.Fatal(err)
	}
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600001.SH", TradeDate: d, Close: 900, RawClose: 900}); err != nil {
		t.Fatal(err)
	}
	// 另一交易日与另一代码的邻日行，都不该混进当日截面。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: "20261120", Close: 1040, RawClose: 1040}); err != nil {
		t.Fatal(err)
	}

	m, err := rc.LimitRowsFor(ctx, d)
	if err != nil {
		t.Fatalf("LimitRowsFor 失败: %v", err)
	}
	if got := limitKeys(m); len(got) != 2 || got[0] != "600000.SH" || got[1] != "600001.SH" {
		t.Fatalf("键集应为当日两根线，实得 %v", got)
	}
	row, ok := m["600999.SH"]
	if ok {
		t.Fatalf("库里没有行的代码出现在 map 里：%+v（缺失键语义被破坏）", row)
	}
	// 邻日行不得串进来：600001.SH 只有 20261123 一行，值必须是那行的 raw_close。
	if int64(m["600001.SH"].RawClose) != 900 {
		t.Fatalf("取到了非当日的行：raw_close=%d", int64(m["600001.SH"].RawClose))
	}
}

// TestLimitRowsForUsesRawCloseBasis 是组长 20261123 裁定三的**接线锁**。封板判定必须拿
// 真实收盘价比真实限价：本用例造一根复权因子≠1 的封板行（前复权 close=2100、
// raw_close=1050、up_limit=1050），使两种基准的判定结果**相反**，于是"误用 close"会
// 当场把测试跑红，而不只是注释里的一句提醒。
//
// 生产实测支撑（20261123 截面）：5,426/5,570 行 close≠raw_close；`raw_close=up_limit`
// 命中 152 只，`close=up_limit` 只剩 2 只 —— 差 150:2，不是理论风险。
func TestLimitRowsForUsesRawCloseBasis(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	rc := s.MarketRepo()

	const d = "20261123"
	// 除权过（因子 2.0）：真实收 1050 == 涨停价 1050 ⇒ 收盘封板成立；前复权 close=2100。
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600000.SH", TradeDate: d, Close: 2100, RawClose: 1050}); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.UpdateBarQuote(ctx, "600000.SH", d, BarQuote{
		Open: fenp(1010), High: fenp(1080), Low: fenp(1000), UpLimit: fenp(1050), DownLimit: fenp(945),
	}); err != nil {
		t.Fatal(err)
	}

	m, err := rc.LimitRowsFor(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := m["600000.SH"]
	if !ok {
		t.Fatal("截面里应有 600000.SH")
	}
	if int64(row.RawClose) != 1050 {
		t.Fatalf("RawClose 必须取 raw_close 列，实得 %d（疑似读成了前复权 close）", int64(row.RawClose))
	}
	if row.UpLimit == nil || int64(*row.UpLimit) != 1050 {
		t.Fatalf("UpLimit 应为 1050，实得 %v", row.UpLimit)
	}
	// 正确基准：判为收盘封板。
	if row.RawClose != *row.UpLimit {
		t.Fatal("raw_close==up_limit 应判为收盘封板")
	}

	// 对照面：close 列确实与 raw_close 不同，且拿它比限价会得到相反结论。
	// 这里把 close 单独从库里读出来（LimitRow 不提供 Close 字段，正是为了让这种
	// 误用需要一个显式的额外查询，而不是顺手就能拿到）。
	var adjusted int64
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT close FROM daily_bar WHERE ts_code='600000.SH' AND trade_date=?`, d).
		Scan(&adjusted); err != nil {
		t.Fatal(err)
	}
	if adjusted == int64(*row.UpLimit) {
		t.Fatal("构造失效：close 与 up_limit 相同就无法区分两种基准")
	}
	if wrongVerdict := adjusted == int64(*row.UpLimit); wrongVerdict {
		t.Fatal("误用前复权 close 的判定不该与正确判定同为真")
	}
}

// TestLimitRowsForUnknownAndIndexRows 锁两件事：
//  1. 限价列为 NULL ⇒ 指针为 nil（unknown），不是 0；
//  2. 指数行以"有 RawClose、两列限价 nil"的形态出现（扩展价族对指数无语义，
//     schema.go 口径 3），调用侧不需要按代码前缀特判。
func TestLimitRowsForUnknownAndIndexRows(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	rc := s.MarketRepo()

	const d = "20261123"
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "600002.SH", TradeDate: d, Close: 800, RawClose: 800}); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.UpdateBarQuote(ctx, "600002.SH", d, BarQuote{
		Open: fenp(790), High: fenp(810), Low: fenp(780), UpLimit: nil, DownLimit: nil,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rc.UpsertBar(ctx, model.Bar{TsCode: "000300.SH", TradeDate: d, Close: 395829, RawClose: 0}); err != nil {
		t.Fatal(err)
	}

	m, err := rc.LimitRowsFor(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if r := m["600002.SH"]; r.UpLimit != nil || r.DownLimit != nil {
		t.Fatalf("NULL 限价必须读成 nil（unknown），实得 up=%v down=%v", r.UpLimit, r.DownLimit)
	}
	idx, ok := m["000300.SH"]
	if !ok {
		t.Fatal("指数行应出现在截面里（它确实有一根线）")
	}
	if idx.RawClose != 0 || idx.UpLimit != nil || idx.DownLimit != nil {
		t.Fatalf("指数行应为 raw_close=0（指数不落真实价）、限价 nil，实得 %+v", idx)
	}
}
