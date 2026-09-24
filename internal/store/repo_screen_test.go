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
