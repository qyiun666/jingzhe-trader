package itest

import (
	"testing"

	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// TestTushareInterfaces 六个数据接口的真实契约（daily / daily_basic / adj_factor /
// suspend_d / index_daily / stock_basic + trade_cal）。
//
// 断言的是"接口今天到底给不给数据、给的是不是全市场"，不是"调用没报错"：
// 本仓库有过 index_daily 用逗号拼多码时静默返回 0 行的事故，0 行看起来和成功一样。
func TestTushareInterfaces(t *testing.T) {
	_, cfg := requireEnv(t)
	ctx := t.Context()
	date := tradeDateOr(t, "20260903")

	cli := tushare.NewClient(cfg.GetString("tushare.token"), cfg.GetString("tushare.base_url"),
		cfg.GetInt("tushare.rate_per_min"))

	bars, err := cli.Daily(ctx, date)
	mustNoErr(t, "daily", err)
	if len(bars) < 5000 {
		t.Fatalf("daily(%s) 应返回全市场（≥5000 行），实际 %d 行", date, len(bars))
	}
	// RawClose 此刻还是 0：未复权价由同步层在乘复权因子前就地填进那一列，接口只给 close。
	var bad int
	for _, b := range bars {
		if b.Close <= 0 || b.TsCode == "" || b.TradeDate != date {
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("daily 有 %d/%d 行缺收盘价或日期不符", bad, len(bars))
	}
	t.Log(describe("daily", len(bars), "异常行", bad))

	dbs, err := cli.DailyBasic(ctx, date)
	mustNoErr(t, "daily_basic", err)
	if len(dbs) < 5000 {
		t.Fatalf("daily_basic(%s) 应返回全市场截面，实际 %d 行", date, len(dbs))
	}

	adjs, err := cli.AdjFactor(ctx, date)
	mustNoErr(t, "adj_factor", err)
	// 复权因子必须覆盖日线：缺一只，那只的当日收盘就会与历史不同基准（同步层现在会因此报错）。
	barSet := make(map[string]bool, len(bars))
	for _, b := range bars {
		barSet[b.TsCode] = true
	}
	adjSet := make(map[string]bool, len(adjs))
	for _, a := range adjs {
		adjSet[a.TsCode] = true
	}
	var missingAdj []string
	for code := range barSet {
		if !adjSet[code] {
			missingAdj = append(missingAdj, code)
		}
	}
	if len(missingAdj) > 0 {
		t.Errorf("adj_factor 未覆盖 %d 只当日有日线的标的（%v），同步层会整体报错", len(missingAdj), firstFew(missingAdj))
	}
	t.Log(describe("adj_factor", len(adjs), "未覆盖", len(missingAdj)))

	susp, err := cli.Suspend(ctx, date)
	mustNoErr(t, "suspend_d", err)
	t.Log(describe("suspend_d", len(susp)))

	// 指数：单码一次调用；逗号拼多码实测返回 0 行，所以调用面必须逐码。
	idx, err := cli.IndexDaily(ctx, date, []string{store.MarketIndex})
	mustNoErr(t, "index_daily", err)
	if len(idx) != 1 || idx[0].TsCode != store.MarketIndex || idx[0].Close <= 0 {
		t.Fatalf("index_daily(%s,%s) 期望 1 行有效收盘，实际 %+v", date, store.MarketIndex, idx)
	}
	multi, err := cli.IndexDaily(ctx, date, []string{store.MarketIndex + ",000001.SH"})
	if err != nil {
		t.Fatalf("index_daily 拼码调用不应报错，实际: %v", err)
	}
	if len(multi) != 0 {
		t.Errorf("拼码调用返回了 %d 行：接口行为已变，可改回一次调用多码（见注释）", len(multi))
	}

	basics, err := cli.StockBasic(ctx)
	mustNoErr(t, "stock_basic", err)
	if len(basics) < 5000 {
		t.Fatalf("stock_basic 应返回全市场在市清单，实际 %d 行", len(basics))
	}
	var noIndustry, noName int
	for _, b := range basics {
		if b.Industry == "" {
			noIndustry++
		}
		if b.Name == "" {
			noName++
		}
	}
	if noIndustry > 0 || noName > 0 {
		t.Errorf("stock_basic 有 %d 行缺行业、%d 行缺名称（板块筛与指令单名称都依赖它们）", noIndustry, noName)
	}
	t.Log(describe("stock_basic", len(basics), "缺行业", noIndustry, "缺名称", noName))
}

// TestTushareTradeCal 交易日历：三个交易所都要有当日行，且未来覆盖够调度与门禁使用。
func TestTushareTradeCal(t *testing.T) {
	st, cfg := requireEnv(t)
	ctx := t.Context()
	date := tradeDateOr(t, "20260903")

	dl := dataloader.New(st, tushare.NewClient(cfg.GetString("tushare.token"),
		cfg.GetString("tushare.base_url"), cfg.GetInt("tushare.rate_per_min")))
	mustNoErr(t, "SyncCalendar", dl.SyncCalendar(ctx))

	rc := st.MarketRepo()
	cal, err := rc.LoadTradeCal(ctx)
	mustNoErr(t, "LoadTradeCal", err)
	if cal[date] != true {
		t.Fatalf("日历里 %s 应为开市日，实际 %v（全部键值见 %d 行）", date, cal[date], len(cal))
	}
	future, err := rc.CountFutureTradeDays(ctx, date)
	mustNoErr(t, "CountFutureTradeDays", err)
	if future < 30 {
		t.Errorf("未来开市日只有 %d 天，低于调度所需的 30 天", future)
	}
	t.Log(describe("日历行数", len(cal), "未来开市", future))
}

func firstFew(items []string) []string {
	if len(items) <= 5 {
		return items
	}
	return items[:5]
}
