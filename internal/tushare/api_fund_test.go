package tushare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"jingzhe-trader/internal/model"
)

// stubServer 按 api_name 回放固定响应，并记录收到的请求（参数与 fields 都要断言：
// ETF 这一族的参数形态与个股不同 —— fund_daily 的 ts_code 是必传项，
// 只给 trade_date 会返回 code=0 + 空行，静默空数据比报错更难发现）。
type stubServer struct {
	srv       *httptest.Server
	requests  []tushareRequest
	responses map[string]*tushareData
}

func newStubServer(t *testing.T, responses map[string]*tushareData) *stubServer {
	t.Helper()
	st := &stubServer{responses: responses}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req tushareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析请求失败: %v", err)
		}
		st.requests = append(st.requests, req)
		data, ok := responses[req.API]
		if !ok {
			data = &tushareData{Fields: []string{}, Items: [][]interface{}{}}
		}
		_ = json.NewEncoder(w).Encode(tushareResponse{Code: 0, Msg: "ok", Data: data})
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func (s *stubServer) client() *Client {
	return NewClient("tok", s.srv.URL, 2000)
}

func barData(items [][]interface{}) *tushareData {
	return &tushareData{Fields: []string{"ts_code", "trade_date", "close", "vol"}, Items: items}
}

// TestFundDailyRangeRequestAndUnits 校验请求参数与解码后的量纲。
//
// 实测锚点（20261008，515050.SH）：fund_daily 返回 close 单位**元**、vol 单位**手**、
// amount 单位**千元**。适配层只取 close/vol 入库，Close 必须是**未复权分**
// （0.916 元 → 92 分），复权由同步侧完成 —— 与个股 Daily 同一约定，
// 两条链共用 daily_bar 的双列语义。
func TestFundDailyRangeRequestAndUnits(t *testing.T) {
	st := newStubServer(t, map[string]*tushareData{
		"fund_daily": barData([][]interface{}{
			[]interface{}{"515050.SH", "20260512", 3.33, 2347666.36},
			[]interface{}{"515050.SH", "20260513", 1.156, 6740545.33},
		}),
	})
	bars, err := st.client().FundDailyRange(context.Background(), "515050.SH", "20260512", "20260513")
	if err != nil {
		t.Fatalf("FundDailyRange: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("行数 %d，期望 2", len(bars))
	}
	if got, want := bars[1].Close, model.Fen(116); got != want {
		t.Errorf("未复权收盘 %d 分，期望 %d 分（1.156 元）", got, want)
	}
	if bars[1].RawClose != 0 {
		t.Errorf("适配层不该就地复权：RawClose=%d，期望 0（复权属同步侧）", bars[1].RawClose)
	}
	if got, want := bars[1].VolLot, 6740545.33; got != want {
		t.Errorf("成交量 %v 手，期望 %v", got, want)
	}
	req := st.requests[0]
	if req.API != "fund_daily" {
		t.Errorf("api_name=%s", req.API)
	}
	if req.Params["ts_code"] != "515050.SH" || req.Params["start_date"] != "20260512" || req.Params["end_date"] != "20260513" {
		t.Errorf("params=%v，期望 ts_code/start_date/end_date", req.Params)
	}
	if req.Fields != "ts_code,trade_date,close,vol" {
		t.Errorf("fields=%q", req.Fields)
	}
}

// TestFundAdjShape fund_adj 与个股 adj_factor 同形，但按日全量返回。
//
// 实测（20220104→20261008，515050.SH）：1151 根日线对应 1151 行因子，缺因子日数 0，
// 所以同步侧可以照旧按"日期→因子"映射逐根复权，不需要前向填充。
func TestFundAdjShape(t *testing.T) {
	st := newStubServer(t, map[string]*tushareData{
		"fund_adj": {Fields: []string{"ts_code", "trade_date", "adj_factor"}, Items: [][]interface{}{
			[]interface{}{"515050.SH", "20260512", 1.0},
			[]interface{}{"515050.SH", "20260513", 3.0},
		}},
	})
	rows, err := st.client().FundAdj(context.Background(), "515050.SH", "20260512", "20260513")
	if err != nil {
		t.Fatalf("FundAdj: %v", err)
	}
	if want := []AdjFactorRow{{TsCode: "515050.SH", TradeDate: "20260512", AdjFactor: 1.0},
		{TsCode: "515050.SH", TradeDate: "20260513", AdjFactor: 3.0}}; !reflect.DeepEqual(rows, want) {
		t.Errorf("因子行 %+v，期望 %+v", rows, want)
	}
}

// TestFundShareUnits fd_share 单位**万份**（实测 515050.SH 20261008 = 1,757,520.54 万份）。
// 规模估算 = fd_share×1e4×单位价，与 dataloader 侧 50 亿那条线同源。
func TestFundShareUnits(t *testing.T) {
	st := newStubServer(t, map[string]*tushareData{
		"fund_share": {Fields: []string{"ts_code", "trade_date", "fd_share"}, Items: [][]interface{}{
			[]interface{}{"515050.SH", "20261008", 1757520.54},
		}},
	})
	rows, err := st.client().FundShare(context.Background(), "515050.SH", "20261001", "20261008")
	if err != nil {
		t.Fatalf("FundShare: %v", err)
	}
	if len(rows) != 1 || rows[0].FDShare != 1757520.54 {
		t.Fatalf("份额行异常: %+v", rows)
	}
}

// TestFundBasicCarriesNameAndDelist fund_basic 的用途是白名单复核：名称要进指令单，
// delist_date 空才是在市。实测本接口**没有跟踪标的字段**，"哪些是科技 ETF"在这里无解，
// 白名单只能人工维护并按 name 逐只核过（515220 是煤炭 ETF 就是这么被剔掉的）。
func TestFundBasicCarriesNameAndDelist(t *testing.T) {
	st := newStubServer(t, map[string]*tushareData{
		"fund_basic": {Fields: []string{"ts_code", "name", "fund_type", "found_date", "delist_date", "manager"},
			Items: [][]interface{}{
				[]interface{}{"515050.SH", "华夏中证5G通信主题ETF", "股票型", "20190917", nil, "华夏基金"},
				[]interface{}{"159001.SZ", "已退市示例", "股票型", "20100101", "20200101", "某基金"},
			}},
	})
	rows, err := st.client().FundBasic(context.Background(), "E")
	if err != nil {
		t.Fatalf("FundBasic: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("行数 %d", len(rows))
	}
	if rows[0].Name != "华夏中证5G通信主题ETF" || rows[0].DelistDate != "" {
		t.Errorf("在市条目解析异常: %+v", rows[0])
	}
	if rows[1].DelistDate != "20200101" {
		t.Errorf("退市日期未落到 DelistDate: %+v", rows[1])
	}
	if st.requests[0].Params["market"] != "E" {
		t.Errorf("params=%v，期望 market=E", st.requests[0].Params)
	}
}
