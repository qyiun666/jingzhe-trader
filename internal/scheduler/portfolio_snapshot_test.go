package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// TestWritePortfolioSnapshot 当日快照必须落 run_trace，且带上 000300 收盘。
func TestWritePortfolioSnapshot(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/snapshot.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()

	const date = "20261023"
	if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
		TsCode: "000300.SH", TradeDate: date, Close: 105000,
	}); err != nil {
		t.Fatalf("写入指数日线失败: %v", err)
	}
	ast := model.Assets{Cash: 1000000, MarketValue: 200000, TotalAsset: 1200000, PositionCount: 1}
	snap, err := writePortfolioSnapshot(ctx, Deps{Store: st}, date, ast)
	if err != nil {
		t.Fatalf("写组合快照失败: %v", err)
	}
	if snap.IndexCloseFen != 105000 {
		t.Errorf("index_close_fen=%d，期望 105000", snap.IndexCloseFen)
	}

	traces, err := st.TraceRepo().List(ctx, date)
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	for _, tr := range traces {
		if tr.Subject != model.TracePortfolioSnapshot() {
			continue
		}
		var got model.PortfolioSnapshot
		if err := json.Unmarshal([]byte(tr.Detail), &got); err != nil {
			t.Fatalf("快照 JSON 解析失败: %v", err)
		}
		if got.TotalAssetFen != 1200000 || got.CashFen != 1000000 || got.MarketValueFen != 200000 {
			t.Errorf("快照资产口径不对: %+v", got)
		}
		if tr.Outcome != model.TraceOK {
			t.Errorf("快照 outcome=%s，期望 ok", tr.Outcome)
		}
		return
	}
	t.Fatal("没有找到 portfolio:snapshot 轨迹")
}

// TestPortfolioPerformanceSection 5/10 日有快照时出对比，20 日缺历史则跳过。
func TestPortfolioPerformanceSection(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/perf.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()

	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var dates []string
	for i := 0; i < 25; i++ {
		d := start.AddDate(0, 0, i).Format("20060102")
		dates = append(dates, d)
		if err := st.MarketRepo().UpsertCal(ctx, store.CalRow{CalDate: d, IsOpen: true}); err != nil {
			t.Fatalf("写交易日历失败: %v", err)
		}
	}
	today := dates[len(dates)-1]

	writeSnap := func(date string, total, index int64) {
		t.Helper()
		payload, _ := json.Marshal(model.PortfolioSnapshot{
			TradeDate: date, TotalAssetFen: total, IndexCloseFen: index,
		})
		if err := st.TraceRepo().Write(ctx, model.RunTrace{
			TradeDate: date, Subject: model.TracePortfolioSnapshot(),
			Outcome: model.TraceOK, Detail: string(payload), At: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("写历史快照失败: %v", err)
		}
	}
	writeSnap(dates[len(dates)-1-5], 1000000, 100000) // 5 日前：组合 10000、指数 1000
	writeSnap(dates[len(dates)-1-10], 900000, 90000)  // 10 日前：组合 9000、指数 900

	if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
		TsCode: "000300.SH", TradeDate: today, Close: 105000, // 指数 +5%
	}); err != nil {
		t.Fatalf("写指数日线失败: %v", err)
	}
	todaySnap, err := writePortfolioSnapshot(ctx, Deps{Store: st}, today,
		model.Assets{Cash: 1100000, TotalAsset: 1100000, PositionCount: 0}) // 组合 +10%
	if err != nil {
		t.Fatalf("写当日快照失败: %v", err)
	}

	section := portfolioPerformanceSection(ctx, Deps{Store: st}, today, todaySnap)
	for _, want := range []string{
		"【组合表现 vs 000300】",
		fmt.Sprintf("5日：组合 +10.00%% vs 000300 +5.00%%（超额 +5.00 pct）"),
		"10日：组合 +22.22% vs 000300 +16.67%（超额 +5.56 pct）",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("组合表现缺少 %q，实际:\n%s", want, section)
		}
	}
	if strings.Contains(section, "20日") {
		t.Errorf("20 日没有历史快照，不应输出该档:\n%s", section)
	}
}
