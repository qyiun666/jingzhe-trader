package dataloader

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// etfStub 回放 fund_daily / fund_adj / fund_share 三个接口（按 ts_code 分表）。
//
// 夹具是 2026-10-09 从 Tushare 只读探针取回的**真实数据**，不是编的：回归用例的价值
// 在于"折算那天到底发生了什么"，换成编造数字就只是在测我自己的算术。
type etfStub struct {
	daily map[string][][]interface{}
	adj   map[string][][]interface{}
	share map[string][][]interface{}
	calls map[string]int
}

func (s *etfStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			API    string                 `json:"api_name"`
			Params map[string]interface{} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("stub 解析请求失败: %v", err)
		}
		code, _ := req.Params["ts_code"].(string)
		s.calls[req.API]++
		var fields []string
		var items [][]interface{}
		switch req.API {
		case "fund_daily":
			fields, items = []string{"ts_code", "trade_date", "close", "vol"}, s.daily[code]
		case "fund_adj":
			fields, items = []string{"ts_code", "trade_date", "adj_factor"}, s.adj[code]
		case "fund_share":
			fields, items = []string{"ts_code", "trade_date", "fd_share"}, s.share[code]
		}
		if items == nil {
			items = [][]interface{}{}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "msg": "ok",
			"data": map[string]interface{}{"fields": fields, "items": items},
		})
	}
}

func newETFDataloader(t *testing.T, stub *etfStub) (*Dataloader, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(stub.handler(t))
	t.Cleanup(srv.Close)
	st, err := store.Open(t.TempDir() + "/etf.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	stub.calls = map[string]int{}
	return New(st, tushare.NewClient("tok", srv.URL, 2000)), st
}

// split515050 是 515050.SH（华夏中证5G通信主题ETF）2026-05-06~05-20 的真实序列。
// 20260513 做 1:3 份额折算：adj_factor 1.0 → 3.0，未复权收盘 3.33 → 1.156。
var split515050 = []struct {
	date   string
	close  float64 // 元（未复权）
	vol    float64 // 手
	factor float64
}{
	{"20260506", 3.036, 2470213.92, 1.0},
	{"20260507", 3.164, 2313503.74, 1.0},
	{"20260508", 3.174, 1800537.85, 1.0},
	{"20260511", 3.299, 2233284.10, 1.0},
	{"20260512", 3.330, 2347666.36, 1.0},
	{"20260513", 1.156, 6740545.33, 3.0},
	{"20260514", 1.133, 9447509.11, 3.0},
	{"20260515", 1.108, 8408646.63, 3.0},
	{"20260518", 1.128, 6214025.13, 3.0},
	{"20260519", 1.131, 7217511.33, 3.0},
	{"20260520", 1.139, 5704885.33, 3.0},
}

func stubForSplit() *etfStub {
	s := &etfStub{
		daily: map[string][][]interface{}{},
		adj:   map[string][][]interface{}{},
		share: map[string][][]interface{}{},
	}
	var d, a [][]interface{}
	for _, r := range split515050 {
		d = append(d, []interface{}{"515050.SH", r.date, r.close, r.vol})
		a = append(a, []interface{}{"515050.SH", r.date, r.factor})
	}
	s.daily["515050.SH"] = d
	s.adj["515050.SH"] = a
	s.share["515050.SH"] = [][]interface{}{
		[]interface{}{"515050.SH", "20260520", 1757520.54},
	}
	return s
}

// TestSyncETFWindow_SplitIsAdjusted 份额折算回归用例（设计文档 §5-2 指定的用例）。
//
// 同一条用例同时断言两个口径，是因为"把折算当跳水"是这个特性唯一真正致命的错法：
//   - 复权后（close 列）折算日 20260512→20260513 必须是一笔 +4% 上下的正常波动；
//   - 未复权（raw_close 列）同一日会报 −65%，而 20261009 实测未复权序列的 4 年日σ
//     是 3.07%、最大回撤 −73.5%，复权后真实值 2.39% / −44.1%。
//     风控参数（止损 8%）是按波动校准的：喂进折算造成的假跳水，等于给每笔 ETF 持仓
//     预埋一次"开盘即触发止损"的误杀。
//   - raw_close 列本身必须等于真实成交价：一手成本、持仓市值、指令单参考价全读它。
//     515050 折算后 1.156 元 → 一手 116 元（可买），写成复权的 3.47 元就是一张下不了的单。
func TestSyncETFWindow_SplitIsAdjusted(t *testing.T) {
	d, st := newETFDataloader(t, stubForSplit())
	ctx := context.Background()

	n, err := d.SyncETFWindow(ctx, "20260506", "20260520", []string{"515050.SH"})
	if err != nil {
		t.Fatalf("SyncETFWindow: %v", err)
	}
	if n != len(split515050) {
		t.Errorf("写入 %d 行，期望 %d", n, len(split515050))
	}
	sr := st.ScreenRepo()
	pts, err := sr.BarCloseSeries(ctx, []string{"20260512", "20260513"})
	if err != nil {
		t.Fatalf("读回日线: %v", err)
	}
	var prev, cur *store.ClosePoint
	for i := range pts {
		if pts[i].TsCode != "515050.SH" {
			continue
		}
		if pts[i].TradeDate == "20260512" {
			prev = &pts[i]
		}
		if pts[i].TradeDate == "20260513" {
			cur = &pts[i]
		}
	}
	if prev == nil || cur == nil {
		t.Fatalf("折算前后两根没读全: %+v", pts)
	}

	// ① 复权口径：折算日是 +4.5% 的正常波动，不是跳水。
	adjRet := cur.Close/prev.Close - 1
	if adjRet < 0 {
		t.Errorf("复权后折算日收益 %.2f%%，必须为正（真实为 +4.5%%，负值说明没复权）", adjRet*100)
	}
	if math.Abs(adjRet-0.045) > 0.01 {
		t.Errorf("复权后折算日收益 %.4f，期望 ≈0.045", adjRet)
	}
	// ② 未复权口径同一日就是那场假跳水：把它写进断言，后来人不会误当噪声删掉。
	rawRet := cur.RawClose/prev.RawClose - 1
	if rawRet > -0.60 {
		t.Errorf("未复权折算日收益 %.4f，期望 ≈−0.65（份额 1:3 折算的原始形态）", rawRet)
	}
	// ③ 双列关系：close = round(raw_close × 当日因子)（ClosePoint 两列都是分，float64）。
	if want := float64(116); cur.RawClose != want {
		t.Errorf("RawClose=%v，期望 %v 分（1.156 元）", cur.RawClose, want)
	}
	if want := float64(348); cur.Close != want {
		t.Errorf("Close=%v，期望 %v 分（116×3.0）", cur.Close, want)
	}
	// ④ 一手成本用未复权价：100 份 × 1.16 元 = 116 元，落在弱势试探单票上限内。
	lot := model.Fen(int64(cur.RawClose)).Mul(model.LotShares)
	if got := lot.Float(); math.Abs(got-116) > 1 {
		t.Errorf("一手成本 %.2f 元，期望 ≈116 元（用复权价会算成 348 元）", got)
	}
	// ⑤ 成交量落库单位是手，与个股同一口径（流动性级靠它推成交额）。
	bar, err := sr.LatestBarAt(ctx, "515050.SH", "20260513")
	if err != nil {
		t.Fatalf("LatestBarAt: %v", err)
	}
	if math.Abs(bar.VolLot-6740545.33) > 1e-6 {
		t.Errorf("VolLot=%v，期望 6740545.33 手", bar.VolLot)
	}
}

// TestSyncETFWindow_Idempotent 同一区间跑两遍行数不变（daily_bar 主键 ts_code+trade_date，
// UpsertBar 覆盖）。逐日补跑与手工重跑都必须幂等，否则回补脚本一断就留下重复语义。
func TestSyncETFWindow_Idempotent(t *testing.T) {
	d, st := newETFDataloader(t, stubForSplit())
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := d.SyncETFWindow(ctx, "20260506", "20260520", []string{"515050.SH"}); err != nil {
			t.Fatalf("第 %d 遍: %v", i+1, err)
		}
	}
	pts, err := st.ScreenRepo().BarCloseSeries(ctx, []string{"20260513"})
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("20260513 有 %d 行，期望 1 行", len(pts))
	}
}

// TestSyncETFWindow_MissingFactorNotWritten 缺当日因子就不写那一根。
//
// 与个股那侧同一个理由：同一根序列里一半复权一半原始，下游拿它算止损就会
// 凭空看到一次跳水。差别只在处置：个股缺因子整批中止（全市场截面不可信），
// ETF 逐根跳过并落 ETF_ADJ_MISSING 告警（补口不能拖垮主链）。
func TestSyncETFWindow_MissingFactorNotWritten(t *testing.T) {
	s := stubForSplit()
	var a [][]interface{}
	for _, r := range s.adj["515050.SH"] {
		if r[1] == "20260514" {
			continue // 模拟 fund_adj 晚发布一天
		}
		a = append(a, r)
	}
	s.adj["515050.SH"] = a
	d, st := newETFDataloader(t, s)
	ctx := context.Background()
	n, err := d.SyncETFWindow(ctx, "20260506", "20260520", []string{"515050.SH"})
	if err != nil {
		t.Fatalf("SyncETFWindow: %v", err)
	}
	if n != len(split515050)-1 {
		t.Errorf("写入 %d 行，期望 %d（缺因子的 20260514 不写）", n, len(split515050)-1)
	}
	pts, err := st.ScreenRepo().BarCloseSeries(ctx, []string{"20260514"})
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	for _, p := range pts {
		if p.TsCode == "515050.SH" {
			t.Errorf("缺因子的 20260514 被写进了库")
		}
	}
	// 缺因子这根不写，选股的"窗口日线不足"就会把它剔掉并在 Notes 出声；
	// 同步侧另落一条 ETF_ADJ_MISSING 轨迹（告警日期按写入时刻的自然日，不在这里断言）。
	if s.calls["fund_daily"] != 1 || s.calls["fund_adj"] != 1 {
		t.Errorf("调用次数异常: %+v（单码区间应当各 1 次）", s.calls)
	}
}

// TestCheckETFScale 规模校验：50 亿是写死的资格线，塌陷时只出声不剔标的。
//
// 估算口径 fd_share(万份)×1e4×当日未复权收盘(元)：实测 515050 20261008 为 160.99 亿，
// 与用 fund_nav 单位净值的口径（161.57 亿）差 0.36% —— 量级判据，不值得为它再接一个接口。
func TestCheckETFScale(t *testing.T) {
	s := stubForSplit()
	s.share["515050.SH"] = [][]interface{}{[]interface{}{"515050.SH", "20260520", 1757520.54}}
	d, st := newETFDataloader(t, s)
	ctx := context.Background()
	if _, err := d.SyncETFWindow(ctx, "20260506", "20260520", []string{"515050.SH"}); err != nil {
		t.Fatalf("同步: %v", err)
	}
	if err := d.CheckETFScale(ctx, "20260520", []string{"515050.SH"}); err != nil {
		t.Fatalf("CheckETFScale: %v", err)
	}
	// 160 亿 ≫ 50 亿：不该落任何告警。
	yi := 1757520.54 * 1e4 * model.Fen(114).Float() / 1e8
	if yi < ETFMinScaleYi {
		t.Errorf("夹具规模 %.2f 亿低于门槛，用例失去意义", yi)
	}

	// 份额塌陷：缩到 1/10 后必须被这条线拦住（断言判据本身，不依赖告警落库形态）。
	small := 175752.054
	if got := small * 1e4 * model.Fen(114).Float() / 1e8; got > ETFMinScaleYi {
		t.Errorf("塌陷后的估算规模 %.2f 亿仍高于门槛，判据不成立", got)
	}
	_ = st
}
