package store

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestLatestMarketIndexWindowParameter 门槛均线窗口参数化后的口径：
// 均线按请求的最近 window 根现算，凑不满给 0（不可算），越界窗口是装配错误直接报。
func TestLatestMarketIndexWindowParameter(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	days := []string{"20260301", "20260302", "20260303"}
	closes := []model.Fen{100, 200, 300}
	for i, d := range days {
		if err := s.MarketRepo().UpsertBar(ctx, model.Bar{
			TsCode: MarketIndex, TradeDate: d, Close: closes[i]}); err != nil {
			t.Fatalf("插入指数日线失败: %v", err)
		}
	}

	cases := []struct {
		window int
		wantMA model.Fen
	}{
		{1, 300}, // 最后一根
		{2, 250}, // (200+300)/2
		{3, 200}, // (100+200+300)/3
		{4, 0},   // 只有 3 根：不可算，而不是拿部分均值当真值
	}
	for _, c := range cases {
		q, err := s.ScreenRepo().LatestMarketIndex(ctx, "20260303", c.window)
		if err != nil {
			t.Fatalf("window=%d 读取失败: %v", c.window, err)
		}
		if q.MA != c.wantMA {
			t.Errorf("window=%d 均线应为 %d，实际 %d", c.window, c.wantMA, q.MA)
		}
		if q.Window != c.window {
			t.Errorf("window=%d 应回显给调用方，实际 %d", c.window, q.Window)
		}
	}

	for _, bad := range []int{0, -1, MarketMAWindow + 1} {
		if _, err := s.ScreenRepo().LatestMarketIndex(ctx, "20260303", bad); err == nil {
			t.Errorf("窗口 %d 越界应报错而不是悄悄按默认算", bad)
		}
	}
}

// TestNextTradeDates 归因前瞻窗口按交易日取，不受周末/节假日影响；
// 日历走完时返回已有的那些，调用方据此判"待到期"。
func TestNextTradeDates(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	for _, d := range []string{"20261009", "20261012", "20261013", "20261014"} {
		if err := s.MarketRepo().UpsertCal(ctx, CalRow{CalDate: d, IsOpen: true}); err != nil {
			t.Fatalf("写日历失败: %v", err)
		}
	}
	got, err := s.ScreenRepo().NextTradeDates(ctx, "20261009", 2)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 2 || got[0] != "20261012" || got[1] != "20261013" {
		t.Errorf("之后 2 个交易日=%v，期望 [20261012 20261013]", got)
	}
	got, err = s.ScreenRepo().NextTradeDates(ctx, "20261014", 3)
	if err != nil || len(got) != 0 {
		t.Errorf("日历走完应返回空，实际 %v err=%v", got, err)
	}
	if got, err := s.ScreenRepo().NextTradeDates(ctx, "20261009", 0); err != nil || len(got) != 0 {
		t.Errorf("n=0 应返回空，实际 %v err=%v", got, err)
	}
}

// TestStockBriefs 批量补名称/行业：只回存在的代码，查无此码不报错（可能是已摘牌标的）。
func TestStockBriefs(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()
	for _, b := range []model.StockBasic{
		{TsCode: "600000.SH", Name: "甲", Industry: "银行", ListStatus: "L"},
		{TsCode: "600001.SH", Name: "乙", Industry: "半导体", ListStatus: "L"},
	} {
		if err := s.MarketRepo().UpsertStockBasic(ctx, b); err != nil {
			t.Fatalf("写股票基础失败: %v", err)
		}
	}
	got, err := s.ScreenRepo().StockBriefs(ctx, []string{"600000.SH", "600001.SH", "999999.SH"})
	if err != nil {
		t.Fatalf("批量读取失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应回 2 只，实际 %d", len(got))
	}
	if got["600000.SH"].Name != "甲" || got["600000.SH"].Industry != "银行" {
		t.Errorf("600000 字段有误: %+v", got["600000.SH"])
	}
	if got["600001.SH"].Industry != "半导体" {
		t.Errorf("600001 行业有误: %+v", got["600001.SH"])
	}
}
