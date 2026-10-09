package store

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestBarsForCodes ETF 小漏斗的读取面：只回给定代码 × 给定日期的交集。
//
// 这条约束要紧：BarCloseSeries 是全市场读法（个股因子横截面用），
// 而 ETF 补口只有白名单几只，逐次全市场读会把 5000+ 只的日线拉进内存再丢掉；
// 更要紧的是"不受历史残留行影响"——窗口日期之外的旧行不得混进流动性计算。
func TestBarsForCodes(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	ctx := context.Background()

	dates := []string{"20260301", "20260302", "20260303"}
	for _, code := range []string{"588000.SH", "512480.SH", "600908.SH"} {
		for _, d := range dates {
			if err := s.MarketRepo().UpsertBar(ctx, model.Bar{
				TsCode: code, TradeDate: d, Close: 91, VolLot: 5e6, RawClose: 91}); err != nil {
				t.Fatalf("写日线失败: %v", err)
			}
		}
		// 窗口外的一天：必须读不到，否则流动性窗口会被历史残留行撑长。
		if err := s.MarketRepo().UpsertBar(ctx, model.Bar{
			TsCode: code, TradeDate: "20260201", Close: 50, VolLot: 1, RawClose: 50}); err != nil {
			t.Fatalf("写窗口外日线失败: %v", err)
		}
	}

	got, err := s.ScreenRepo().BarsForCodes(ctx, []string{"588000.SH", "512480.SH"}, dates)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("两只 × 三日 = 6 行，实际 %d：%+v", len(got), got)
	}
	for _, p := range got {
		if p.TsCode == "600908.SH" || p.TradeDate == "20260201" {
			t.Errorf("越界行混入结果：%+v", p)
		}
	}
	// 按代码、日期升序：RunETF 依赖"末元素就是当日那一根"来判新鲜度。
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if prev.TsCode > cur.TsCode || (prev.TsCode == cur.TsCode && prev.TradeDate >= cur.TradeDate) {
			t.Errorf("升序被破坏：%+v 在 %+v 之后", prev, cur)
		}
	}

	// 空入参不是"全表扫描"，而是空结果。
	if rows, err := s.ScreenRepo().BarsForCodes(ctx, nil, dates); err != nil || len(rows) != 0 {
		t.Errorf("代码集为空应返回空，实际 %d 行 err=%v", len(rows), err)
	}
	if rows, err := s.ScreenRepo().BarsForCodes(ctx, []string{"588000.SH"}, nil); err != nil || len(rows) != 0 {
		t.Errorf("日期集为空应返回空，实际 %d 行 err=%v", len(rows), err)
	}
}

// TestBarsForCodesUnknownCode 白名单里某只从没同步过：返回空而不是报错，
// 由 RunETF 的资格级把它按"窗口日线不足"剔掉并计数。
func TestBarsForCodesUnknownCode(t *testing.T) {
	s := openStoreForTest(t)
	defer s.Close()
	rows, err := s.ScreenRepo().BarsForCodes(context.Background(),
		[]string{"159995.SZ"}, []string{"20260301"})
	if err != nil {
		t.Fatalf("查无此码不应报错: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("应为空，实际 %+v", rows)
	}
}
