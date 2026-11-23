package dataloader

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// quoteStub 回放 syncOneDay 用到的五个接口，并可让其中 stk_limit 单独失败。
//
// 夹具的字段顺序与 tushare 客户端请求的 fields 一致；价格是**元**（真实口径），
// 由解码层转成分——这样"元→分"的边界也被这条用例覆盖着。
type quoteStub struct {
	daily   [][]interface{} // ts_code, trade_date, close, vol, open, high, low
	basic   [][]interface{} // ts_code, turnover_rate, pe_ttm, pb, circ_mv
	adj     [][]interface{} // ts_code, trade_date, adj_factor
	suspend [][]interface{} // ts_code
	limit   [][]interface{} // ts_code, trade_date, up_limit, down_limit

	failLimit bool // stk_limit 单独报错：T14 用它验"降级不中断"
	calls     map[string]int
}

var quoteStubFields = map[string][]string{
	"daily":       {"ts_code", "trade_date", "close", "vol", "open", "high", "low"},
	"daily_basic": {"ts_code", "turnover_rate", "pe_ttm", "pb", "circ_mv"},
	"adj_factor":  {"ts_code", "trade_date", "adj_factor"},
	"suspend_d":   {"ts_code"},
	"stk_limit":   {"ts_code", "trade_date", "up_limit", "down_limit"},
}

func (s *quoteStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			API string `json:"api_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("stub 解析请求失败: %v", err)
		}
		s.calls[req.API]++
		if req.API == "stk_limit" && s.failLimit {
			// 40203 = 无权限，Classify 归永久（不重试，测试不会因为退避而变慢）。
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 40203, "msg": "没有接口访问权限", "data": nil,
			})
			return
		}
		var items [][]interface{}
		switch req.API {
		case "daily":
			items = s.daily
		case "daily_basic":
			items = s.basic
		case "adj_factor":
			items = s.adj
		case "suspend_d":
			items = s.suspend
		case "stk_limit":
			items = s.limit
		}
		if items == nil {
			items = [][]interface{}{}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "msg": "ok",
			"data": map[string]interface{}{"fields": quoteStubFields[req.API], "items": items},
		})
	}
}

func newQuoteDataloader(t *testing.T, stub *quoteStub) (*Dataloader, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)
	st, err := store.Open(t.TempDir() + "/quote.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	stub.calls = map[string]int{}
	return New(st, tushare.NewClient("tok", srv.URL, 2000)), st
}

// quoteRow 读回一根 bar 的全部相关列；扩展价族列 NULL 一律回 nil。
type quoteRow struct {
	Close, RawClose int64
	Open, High, Low *int64
	UpLimit, Down   *int64
}

func readBar(t *testing.T, st *store.Store, tsCode, date string) (quoteRow, bool) {
	t.Helper()
	var (
		cl, raw         int64
		o, h, l, up, dn sql.NullInt64
	)
	err := st.ReadDB().QueryRowContext(context.Background(),
		`SELECT close, raw_close, open, high, low, up_limit, down_limit
			FROM daily_bar WHERE ts_code=? AND trade_date=?`, tsCode, date).
		Scan(&cl, &raw, &o, &h, &l, &up, &dn)
	if err == sql.ErrNoRows {
		return quoteRow{}, false
	}
	if err != nil {
		t.Fatalf("读 bar 失败: %v", err)
	}
	pick := func(v sql.NullInt64) *int64 {
		if !v.Valid {
			return nil
		}
		n := v.Int64
		return &n
	}
	return quoteRow{Close: cl, RawClose: raw, Open: pick(o), High: pick(h), Low: pick(l),
		UpLimit: pick(up), Down: pick(dn)}, true
}

func mustVal(t *testing.T, col string, p *int64, want int64) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s 是 NULL（unknown），期望 %d", col, want)
	}
	if *p != want {
		t.Errorf("%s = %d，期望 %d", col, *p, want)
	}
}

func mustNull(t *testing.T, col string, p *int64) {
	t.Helper()
	if p != nil {
		t.Errorf("%s = %d，期望 NULL（unknown 不得记 0）", col, *p)
	}
}

// baseStub 造一个三只票的交易日：A 有完整 OHLC+限价（且复权因子 2.0，用来钉基准）、
// B 的 OHLC 接口给 0（必须落 NULL）、C 有 OHLC 但没有限价行。
// 另外 stk_limit 多返回一只 D（daily 里没有）——它绝不能在 daily_bar 造出假 bar。
func baseStub() *quoteStub {
	return &quoteStub{
		daily: [][]interface{}{
			{"600000.SH", "20260901", 10.50, 5.0, 10.10, 10.80, 10.00},
			{"600001.SH", "20260901", 20.00, 1.0, 0.0, 0.0, 0.0},
			{"600002.SH", "20260901", 5.00, 2.0, 4.90, 5.10, 4.80},
		},
		basic: [][]interface{}{
			{"600000.SH", 1.2, 10.0, 1.0, 100000.0},
			{"600001.SH", 1.3, 11.0, 1.1, 110000.0},
			{"600002.SH", 1.4, 12.0, 1.2, 120000.0},
		},
		adj: [][]interface{}{
			{"600000.SH", "20260901", 2.0},
			{"600001.SH", "20260901", 1.0},
			{"600002.SH", "20260901", 1.0},
		},
		limit: [][]interface{}{
			{"600000.SH", "20260901", 11.55, 9.45},
			{"600002.SH", "20260901", 5.50, 4.50},
			{"600999.SH", "20260901", 3.30, 2.70}, // daily 里没有这只
		},
	}
}

// TestSyncOneDayFillsExtendedQuotes 数据面 5 列的填充语义：
// OHLC 与限价按 UPDATE-only 落到既有行；接口没给的（OHLC 为 0、无限价行）一律 NULL；
// 限价接口多出的代码不得凭空造出一根 bar；Close 复权而扩展价族**不复权**。
func TestSyncOneDayFillsExtendedQuotes(t *testing.T) {
	ctx := context.Background()
	stub := baseStub()
	d, st := newQuoteDataloader(t, stub)
	rc := st.MarketRepo()
	// SaveValuation 是更新 stock_basic 的既有行（不在 stock_basic 里的代码不进截面），
	// 所以先用正常路径放一只票进来，好在同步之后核对"估值链路没被新列影响"。
	if err := rc.UpsertStockBasic(ctx, model.StockBasic{
		TsCode: "600000.SH", Name: "浦发银行", Industry: "银行", ListStatus: "L"}); err != nil {
		t.Fatalf("造 stock_basic 失败: %v", err)
	}

	if err := d.syncOneDay(ctx, rc, "20260901"); err != nil {
		t.Fatalf("syncOneDay 失败: %v", err)
	}
	if stub.calls["stk_limit"] != 1 {
		t.Errorf("stk_limit 调用 %d 次，期望 1（每日一次全市场）", stub.calls["stk_limit"])
	}

	var n int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM daily_bar`).Scan(&n); err != nil {
		t.Fatalf("数行失败: %v", err)
	}
	if n != 3 {
		t.Errorf("daily_bar %d 行，期望 3 —— 限价接口多出的 600999.SH 造出了假 bar", n)
	}
	if _, ok := readBar(t, st, "600999.SH", "20260901"); ok {
		t.Error("600999.SH 出现在 daily_bar：UPDATE-only 被破坏（假 bar）")
	}

	a, ok := readBar(t, st, "600000.SH", "20260901")
	if !ok {
		t.Fatal("600000.SH 没写进去")
	}
	// 基准一致性：Close 是前复权（10.50×2.0=2100 分），而 OHLC/限价保持交易所真实价。
	if a.Close != 2100 {
		t.Errorf("close = %d，期望 2100（前复权）", a.Close)
	}
	if a.RawClose != 1050 {
		t.Errorf("raw_close = %d，期望 1050", a.RawClose)
	}
	mustVal(t, "open", a.Open, 1010)
	mustVal(t, "high", a.High, 1080)
	mustVal(t, "low", a.Low, 1000)
	mustVal(t, "up_limit", a.UpLimit, 1155)
	mustVal(t, "down_limit", a.Down, 945)

	b, ok := readBar(t, st, "600001.SH", "20260901")
	if !ok {
		t.Fatal("600001.SH 没写进去")
	}
	if b.Close != 2000 {
		t.Errorf("close = %d，期望 2000", b.Close)
	}
	mustNull(t, "open", b.Open)
	mustNull(t, "high", b.High)
	mustNull(t, "low", b.Low)
	mustNull(t, "up_limit", b.UpLimit)
	mustNull(t, "down_limit", b.Down)

	c, ok := readBar(t, st, "600002.SH", "20260901")
	if !ok {
		t.Fatal("600002.SH 没写进去")
	}
	mustVal(t, "open", c.Open, 490)
	mustVal(t, "up_limit", c.UpLimit, 550)
	mustVal(t, "down_limit", c.Down, 450)

	// 既有链路不能被新列影响：估值截面照写。
	var valDate string
	if err := st.ReadDB().QueryRowContext(ctx,
		`SELECT val_date FROM stock_basic WHERE ts_code='600000.SH'`).Scan(&valDate); err != nil {
		t.Fatalf("读估值截面失败: %v", err)
	}
	if valDate != "20260901" {
		t.Errorf("val_date = %s，期望 20260901（估值链路被新列改动影响）", valDate)
	}
}

// TestSyncOneDayStkLimitFailureDegradesNotFatal T14（验收硬项）：
// 新数据源 stk_limit 失败时，syncOneDay 必须**照常返回 nil**、五列留 NULL，
// 并且留一条可见的告警行 —— 一根限价线的缺失不得打断收盘链（估值、停牌、选股照跑）。
func TestSyncOneDayStkLimitFailureDegradesNotFatal(t *testing.T) {
	ctx := context.Background()
	stub := baseStub()
	stub.failLimit = true
	d, st := newQuoteDataloader(t, stub)
	rc := st.MarketRepo()

	if err := d.syncOneDay(ctx, rc, "20260901"); err != nil {
		t.Fatalf("stk_limit 失败被当成致命错误抛出（买入链会因此中断）: %v", err)
	}

	// 日线照写、限价列全 NULL。
	var n int
	if err := st.ReadDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM daily_bar`).Scan(&n); err != nil {
		t.Fatalf("数行失败: %v", err)
	}
	if n != 3 {
		t.Errorf("daily_bar %d 行，期望 3（日线侧不受限价失败影响）", n)
	}
	for _, code := range []string{"600000.SH", "600001.SH", "600002.SH"} {
		row, ok := readBar(t, st, code, "20260901")
		if !ok {
			t.Fatalf("%s 没写进去", code)
		}
		mustNull(t, code+".up_limit", row.UpLimit)
		mustNull(t, code+".down_limit", row.Down)
	}
	// OHLC 来自 daily，与限价失败无关，必须照填。
	if row, ok := readBar(t, st, "600000.SH", "20260901"); !ok {
		t.Fatal("600000.SH 丢了")
	} else {
		mustVal(t, "open", row.Open, 1010)
	}

	// 下游接口照调（降级不是提前 return）。
	if stub.calls["suspend_d"] != 1 || stub.calls["daily_basic"] != 1 {
		t.Errorf("后续接口调用数异常 suspend_d=%d daily_basic=%d，期望各 1",
			stub.calls["suspend_d"], stub.calls["daily_basic"])
	}

	// 失败面必须留痕，不能静默。
	today := time.Now().In(market.Loc).Format("20060102")
	rows, err := st.TraceRepo().List(ctx, today)
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	var found *model.RunTrace
	for i := range rows {
		if strings.HasPrefix(rows[i].Subject, "alert:TUSHARE_LIMIT_SKIPPED") {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("没有降级告警行，实际 %d 条轨迹（失败被静默吞掉）", len(rows))
	}
	if found.Outcome != model.TraceFail {
		t.Errorf("告警行 outcome = %s，期望 fail", found.Outcome)
	}
	if !strings.Contains(found.Detail, "40203") {
		t.Errorf("告警行 detail 未带原始错误码：%q", found.Detail)
	}
}

// TestBarQuoteZeroPriceMeansUnknown 钉住 fenPtr/barQuoteOf 的三态判据：
// 0 价（接口对停牌/无成交就是这么给的）与缺失同义，一律 nil。
func TestBarQuoteZeroPriceMeansUnknown(t *testing.T) {
	bar := model.Bar{Open: 0, High: 1080, Low: 1000}
	lim := tushare.StkLimitRow{UpLimit: 0, DownLimit: 945}

	q := barQuoteOf(bar, lim, true)
	if q.Open != nil {
		t.Errorf("open=0 被写成 %d，应为 nil（unknown）", int64(*q.Open))
	}
	if q.UpLimit != nil {
		t.Errorf("up_limit=0 被写成 %d，应为 nil（unknown）", int64(*q.UpLimit))
	}
	mustVal(t, "high", int64p(q.High), 1080)
	mustVal(t, "down_limit", int64p(q.DownLimit), 945)

	// hasLim=false（stk_limit 没这只）时两列都必须 unknown，即使结构体零值看着"有数"。
	q2 := barQuoteOf(bar, tushare.StkLimitRow{}, false)
	mustNull(t, "up_limit", int64p(q2.UpLimit))
	mustNull(t, "down_limit", int64p(q2.DownLimit))
}

func int64p(v *model.Fen) *int64 {
	if v == nil {
		return nil
	}
	n := int64(*v)
	return &n
}
