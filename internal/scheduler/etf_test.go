package scheduler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// TestETFCodesOf 白名单 → 代码列（同步侧只需要代码）。
func TestETFCodesOf(t *testing.T) {
	got := etfCodesOf(fakeConfig{"screen.etf_whitelist": "515050.SH:华夏中证5G通信主题ETF:通信设备,159819.SZ:易方达中证人工智能主题ETF:人工智能"})
	if len(got) != 2 || got[0] != "159819.SZ" || got[1] != "515050.SH" {
		t.Errorf("codes=%v，期望升序两只", got)
	}
	if n := len(etfCodesOf(fakeConfig{})); n != 0 {
		t.Errorf("键缺失应给出空池，实际 %v 个代码", n)
	}
	// 重复代码只留一个：同一标的在同一次评审里出现两次会让权重被双算。
	dup := etfCodesOf(fakeConfig{"screen.etf_whitelist": "515050.SH:甲,515050.sh:乙"})
	if len(dup) != 1 {
		t.Errorf("重复代码没去重: %v", dup)
	}
}

// TestSyncETFBarsGateOffIsNoOp 总闸关闭时这段必须一次网络调用都不发。
//
// 这就是"部署当日零行为变化"的可执行定义：合上 ETF 链的全部代码进主干那天，
// 生产进程的行为差别 = 零。开闸是组长的一次 config set，不是一次部署。
func TestSyncETFBarsGateOffIsNoOp(t *testing.T) {
	d, st, calls := etfDeps(t, "20260513")
	dep := Deps{Store: st, Dataloader: d, Config: fakeConfig{"screen.etf_enabled": "false"}}
	rc := observability.NewRunCtx(context.Background(), "evening_pipeline", "20260513")
	dep.syncETFBars(context.Background(), rc, "20260513")
	if n := calls.count("fund_daily"); n != 0 {
		t.Errorf("总闸关闭却发了 %d 次 fund_daily 调用", n)
	}
	if pts, err := st.ScreenRepo().BarCloseSeries(context.Background(), []string{"20260513"}); err != nil {
		t.Fatalf("读库: %v", err)
	} else if len(pts) != 0 {
		t.Errorf("总闸关闭却写入了 %d 行日线", len(pts))
	}
}

// TestSyncETFBarsGateOnWritesBars 开闸后走的就是夜间同一条 SyncETF 路径（幂等写 daily_bar）。
func TestSyncETFBarsGateOnWritesBars(t *testing.T) {
	d, st, calls := etfDeps(t, "20260513")
	dep := Deps{
		Store: st, Dataloader: d,
		Config: fakeConfig{
			"screen.etf_enabled":           "true",
			"screen.etf_whitelist":         "515050.SH:华夏中证5G通信主题ETF:通信设备",
			"screen.etf_min_avg_amount_yi": "2.0",
		},
	}
	rc := observability.NewRunCtx(context.Background(), "evening_pipeline", "20260513")
	dep.syncETFBars(context.Background(), rc, "20260513")

	if calls.count("fund_daily") != 1 || calls.count("fund_adj") != 1 || calls.count("fund_share") != 1 {
		t.Errorf("当日增量应各 1 次调用，实际 daily=%d adj=%d share=%d",
			calls.count("fund_daily"), calls.count("fund_adj"), calls.count("fund_share"))
	}
	bar, err := st.ScreenRepo().LatestBarAt(context.Background(), "515050.SH", "20260513")
	if err != nil {
		t.Fatalf("ETF 日线没落库: %v", err)
	}
	if int64(bar.RawClose) != 116 || int64(bar.Close) != 348 {
		t.Errorf("落库双列 raw=%d close=%d，期望 116 / 348（折算日复权口径）", int64(bar.RawClose), int64(bar.Close))
	}
}

// etfCallLog 记录 stub 收到的各接口次数（"没调用"与"调了几次"都是被测行为）。
type etfCallLog struct {
	m map[string]int
}

func (l *etfCallLog) count(api string) int { return l.m[api] }

// etfDeps 起一个只认 515050.SH 20260512/13 两根线的桩服务 + 空库。
func etfDeps(t *testing.T, date string) (*dataloader.Dataloader, *store.Store, *etfCallLog) {
	t.Helper()
	log := &etfCallLog{m: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			API string `json:"api_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		log.m[req.API]++
		var fields []string
		var items [][]interface{}
		switch req.API {
		case "fund_daily":
			fields, items = []string{"ts_code", "trade_date", "close", "vol"},
				[][]interface{}{[]interface{}{"515050.SH", date, 1.156, 6740545.33}}
		case "fund_adj":
			fields, items = []string{"ts_code", "trade_date", "adj_factor"},
				[][]interface{}{[]interface{}{"515050.SH", date, 3.0}}
		case "fund_share":
			fields, items = []string{"ts_code", "trade_date", "fd_share"},
				[][]interface{}{[]interface{}{"515050.SH", date, 1757520.54}}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "msg": "ok",
			"data": map[string]interface{}{"fields": fields, "items": items},
		})
	}))
	t.Cleanup(srv.Close)
	st, err := store.Open(t.TempDir() + "/etf-sched.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return dataloader.New(st, tushare.NewClient("tok", srv.URL, 2000)), st, log
}
