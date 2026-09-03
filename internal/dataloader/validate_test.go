package dataloader

import (
	"context"
	"testing"

	"jingzhe-trader/internal/model"
)

// TestFinaVisibleAsOf 验证 point-in-time 过滤（验收 #7）：
// 公告日 > 基准日的财报必须被剔除，杜绝前视偏差；空公告日视为已公开保留。
func TestFinaVisibleAsOf(t *testing.T) {
	asOf := "20240101"
	rows := []model.FinaIndicator{
		{TsCode: "600519.SH", EndDate: "20231231", AnnDate: "20240101"}, // 恰好等于基准：保留
		{TsCode: "600520.SH", EndDate: "20230930", AnnDate: "20231201"}, // 早于基准：保留
		{TsCode: "600521.SH", EndDate: "20231231", AnnDate: "20240201"}, // 晚于基准：剔除（捏造的未来财报）
		{TsCode: "600522.SH", EndDate: "20230630", AnnDate: ""},         // 空公告日：保留（无法判定为未来）
	}
	got := FinaVisibleAsOf(rows, asOf)
	if len(got) != 3 {
		t.Fatalf("期望保留 3 条（剔除 1 条未来财报），实际 %d: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, r := range got {
		seen[r.TsCode] = true
	}
	if seen["600521.SH"] {
		t.Fatal("未来财报 600521.SH 不应通过 point-in-time 过滤")
	}
	for _, c := range []string{"600519.SH", "600520.SH", "600522.SH"} {
		if !seen[c] {
			t.Fatalf("合法财报 %s 应被保留", c)
		}
	}
}

// TestCheckAdjFactor 验证复权一致性校验（验收 #8）：
// 期望 close == round(raw_close × adj_factor)；不符则计入异常并返回 ts_code。
func TestCheckAdjFactor(t *testing.T) {
	st := openTestStore(t)
	rc := st.MarketRepo()
	tradeDate := "20260901"

	// 干净数据：close == raw_close × adj_factor（adj=1）
	if err := rc.UpsertBar(context.Background(), model.Bar{
		TsCode: "600519.SH", TradeDate: tradeDate,
		Open: model.FromFloat(100), High: model.FromFloat(101), Low: model.FromFloat(99),
		Close: model.FromFloat(100), PreClose: model.FromFloat(100), RawClose: model.FromFloat(100), AdjFactor: 1.0,
	}); err != nil {
		t.Fatalf("插入干净日线失败: %v", err)
	}
	// 脏数据：close 与 raw_close × adj_factor 不一致（应为 100，被篡改为 99）
	if err := rc.UpsertBar(context.Background(), model.Bar{
		TsCode: "600520.SH", TradeDate: tradeDate,
		Open: model.FromFloat(99), High: model.FromFloat(99), Low: model.FromFloat(99),
		Close: model.FromFloat(99), PreClose: model.FromFloat(100), RawClose: model.FromFloat(100), AdjFactor: 1.0,
	}); err != nil {
		t.Fatalf("插入脏日线失败: %v", err)
	}

	d := New(st, nil)
	anom, err := d.CheckAdjFactor(context.Background(), tradeDate)
	if err != nil {
		t.Fatalf("CheckAdjFactor 失败: %v", err)
	}
	if len(anom) != 1 || anom[0] != "600520.SH" {
		t.Fatalf("期望仅 600520.SH 为异常，实际 %+v", anom)
	}
}
