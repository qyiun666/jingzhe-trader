package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// newTestServerForFields 复用 smoke_test 的脚手架，构造带最小依赖的测试服务
func newTestServerForFields(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	t.Setenv("JZ_SERVER_API_TOKEN", "testtoken")

	st, err := store.Open(filepath.Join(t.TempDir(), "fields.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	deps := Deps{
		Store:     st,
		Config:    cfg,
		Goal:      goal.NewService(st, goal.DefaultConfig(), ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000))),
		Freshness: dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), 0),
		Ledger:    ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000)),
		Tickets:   ticket.NewService(st),
	}
	srv, err := New(deps, "testtoken")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	return ts, func() { ts.Close(); st.Close() }
}

// ============================================================================
// 1. 必填参数缺失校验（所有写工具）
// ============================================================================

func TestField_MissingRequiredParams(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	cases := []struct {
		name    string
		method  string
		params  map[string]interface{}
		wantErr bool
	}{
		// sync_portfolio: positions + available_cash_yuan 必填
		{"sync_portfolio 缺 positions", "sync_portfolio", map[string]interface{}{"available_cash_yuan": 10000}, true},
		{"sync_portfolio positions 空数组", "sync_portfolio", map[string]interface{}{"available_cash_yuan": 10000, "positions": []interface{}{}}, true},
		{"sync_portfolio 缺 available_cash_yuan", "sync_portfolio", map[string]interface{}{"positions": []map[string]interface{}{{"ts_code": "600519.SH"}}}, true},
		{"sync_portfolio available_cash_yuan=0", "sync_portfolio", map[string]interface{}{"positions": []map[string]interface{}{{"ts_code": "600519.SH"}}, "available_cash_yuan": 0}, true},

		// report_fill: qty + price + (ticket_id|ts_code)
		{"report_fill 缺 qty", "report_fill", map[string]interface{}{"price": 100, "ticket_id": 1}, true},
		{"report_fill 缺 price", "report_fill", map[string]interface{}{"qty": 1000, "ticket_id": 1}, true},
		{"report_fill 缺 ticket_id 和 ts_code", "report_fill", map[string]interface{}{"qty": 1000, "price": 100}, true},

		// skip_ticket: ticket_id + reason
		{"skip_ticket 缺 ticket_id", "skip_ticket", map[string]interface{}{"reason": "test"}, true},
		{"skip_ticket 缺 reason", "skip_ticket", map[string]interface{}{"ticket_id": 1}, true},

		// set_gear: gear + reason
		{"set_gear 缺 gear", "set_gear", map[string]interface{}{"reason": "test"}, true},
		{"set_gear 缺 reason", "set_gear", map[string]interface{}{"gear": "G1"}, true},

		// trigger_task: task 必填。缺失必须在进 handler 之前被拒 —
		// 否则它会先摸到未注入的 Deps.Jobs 并 panic（HTTP 层恢复后客户端只拿到断连）。
		{"trigger_task 缺 task", "trigger_task", map[string]interface{}{}, true},

		// init_day / confirm_pace: date 非必填
		{"init_day 无参", "init_day", map[string]interface{}{}, false},
		{"confirm_pace 无参", "confirm_pace", map[string]interface{}{}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{"name": c.method, "arguments": c.params})
			out, _ := res["result"].(map[string]interface{})
			if out == nil {
				t.Fatalf("%s 无 result: %v", c.name, res)
			}
			if isErr := out["isError"] == true; isErr != c.wantErr {
				t.Errorf("%s 期望错误=%v，实际 isError=%v（text=%v）", c.name, c.wantErr, isErr, toolText(out))
			}
		})
	}
}

// toolText 取工具返回的文本，失败时让断言信息能直接看到原因。
func toolText(out map[string]interface{}) string {
	list, _ := out["content"].([]interface{})
	if len(list) == 0 {
		return ""
	}
	first, _ := list[0].(map[string]interface{})
	return fmt.Sprint(first["text"])
}

// ============================================================================
// 2. 非法枚举值与格式校验
// ============================================================================

func TestField_InvalidEnumAndFormat(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	cases := []struct {
		name   string
		method string
		params map[string]interface{}
	}{
		// get_tickets: status 非法
		{"get_tickets status=rejected", "get_tickets", map[string]interface{}{"date": "20260901", "status": "rejected"}},
		{"get_tickets status=invalid", "get_tickets", map[string]interface{}{"date": "20260901", "status": "invalid_status"}},

		// set_gear: gear 非法
		{"set_gear gear=G0", "set_gear", map[string]interface{}{"gear": "G0", "reason": "test"}},
		{"set_gear gear=X", "set_gear", map[string]interface{}{"gear": "X", "reason": "test"}},

		// report_fill: price 负数
		{"report_fill price<0", "report_fill", map[string]interface{}{"ticket_id": 1, "qty": 1000, "price": -100}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{"name": c.method, "arguments": c.params})
			out, _ := res["result"].(map[string]interface{})
			if out == nil {
				t.Fatalf("%s 无 result: %v", c.name, res)
			}
			if out["isError"] != true {
				t.Errorf("%s 期望业务报错，实际 isError=false, result=%v", c.name, out)
			}
		})
	}
}

// TestField_BadDateFormatRejected 日期参数必须在分发前拒掉。
//
// 下游 market.QuarterOf / PrevTradeDay 按 date[:4] 定长切片：短一个字符就在纯函数里 panic，
// 被调度器 recover 之后只剩一条看不出所以然的失败；读工具则会把它当成「当天没有数据」
// 回一个空列表 —— 对外部 agent 来说那是假绿。
func TestField_BadDateFormatRejected(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	cases := []struct {
		method string
		args   map[string]interface{}
	}{
		{"get_tickets", map[string]interface{}{"date": "2026"}},
		{"get_tickets", map[string]interface{}{"date": "2026-09-01"}},
		{"get_tickets", map[string]interface{}{"date": "20260230"}},
		{"get_tickets", map[string]interface{}{"date": "abc"}},
		{"get_logs", map[string]interface{}{"date": "2026"}},
		{"trigger_task", map[string]interface{}{"task": "daily_report", "date": "2026"}},
		{"set_gear", map[string]interface{}{"gear": "G1", "reason": "覆盖到期日写错", "until": "2026-12-31"}},
	}
	for _, c := range cases {
		name := c.method + " date=" + fmt.Sprint(c.args["date"])
		t.Run(name, func(t *testing.T) {
			res := mcpCall(t, ts, "testtoken", "tools/call",
				map[string]interface{}{"name": c.method, "arguments": c.args})
			out, _ := res["result"].(map[string]interface{})
			if out == nil {
				t.Fatalf("%s 无 result: %v", name, res)
			}
			if out["isError"] != true {
				t.Errorf("%s 竟然被接受，返回 %v", name, toolText(out))
				return
			}
			if txt := toolText(out); !strings.Contains(txt, "YYYYMMDD") {
				t.Errorf("%s 的拒绝信息没写清格式要求: %s", name, txt)
			}
		})
	}
}

// TestField_ArrayItemRequiredRejected sync_portfolio 数组项的必填字段必须在服务端拦住。
//
// 顶层 required 校验管不到嵌套：漏传 `total_qty` 以前会落进 argInt 的缺省 0，
// 于是"这只票清仓了"被当成一次合法校准写进账本，响应依然是成功（synced:1），
// 1200 股的真实持仓就这么被静默清零。所以除了"被拒"，还要断言账本没被动过。
func TestField_ArrayItemRequiredRejected(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	const code = "600519.SH"
	// syncCall 做一次组合同步，直接返回 JSON-RPC 的 result 段
	syncCall := func(row map[string]interface{}) map[string]interface{} {
		res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{
			"name": "sync_portfolio",
			"arguments": map[string]interface{}{
				"available_cash_yuan": 5000.0,
				"positions":           []interface{}{row},
			},
		})
		out, _ := res["result"].(map[string]interface{})
		return out
	}
	// 前置：先做一次合法校准，账本上留下 300 股
	if out := syncCall(map[string]interface{}{"ts_code": code, "total_qty": 300.0, "cost_price": 1500.0}); out == nil || out["isError"] == true {
		t.Fatalf("前置合法校准失败: %v", out)
	}

	for _, c := range []struct {
		name string
		row  map[string]interface{}
	}{
		{"缺 total_qty", map[string]interface{}{"ts_code": code, "cost_price": 1500.0}},
		{"缺 ts_code", map[string]interface{}{"total_qty": 300.0}},
		{"ts_code 为空串", map[string]interface{}{"ts_code": "", "total_qty": 300.0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := syncCall(c.row)
			if out == nil || out["isError"] != true {
				t.Fatalf("%s 竟然被接受: %v", c.name, out)
			}
			if txt := toolText(out); !strings.Contains(txt, "必填字段") || !strings.Contains(txt, "第 1 项") {
				t.Errorf("拒绝信息没指名第几项缺哪个字段: %s", txt)
			}
			if got := positionQty(t, ts, code); got != 300 {
				t.Errorf("被拒的调用改动了账本：%s 持仓变成 %d 股（应仍是 300）", code, got)
			}
		})
	}

	// 显式给 0 是合法校准（券商那边确实卖光了），不能被这道校验误伤
	if out := syncCall(map[string]interface{}{"ts_code": code, "total_qty": 0.0, "cost_price": 1500.0}); out == nil || out["isError"] == true {
		t.Errorf("显式 total_qty:0 的清仓校准被误拒: %v", out)
	}
}

// positionQty 走 MCP get_positions 读某只票当前持仓股数（与外部 agent 看到的同一份口径）。
// 账本里没有这只票返回 -1。
func positionQty(t *testing.T, ts *httptest.Server, code string) int64 {
	t.Helper()
	res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{
		"name": "get_positions", "arguments": map[string]interface{}{}})
	out, _ := res["result"].(map[string]interface{})
	if out == nil || out["isError"] == true {
		t.Fatalf("get_positions 调用失败: %v", res)
	}
	var p struct {
		Positions []struct {
			TsCode   string `json:"TsCode"`
			TotalQty int64  `json:"TotalQty"`
		} `json:"positions"`
	}
	if err := json.Unmarshal([]byte(toolText(out)), &p); err != nil {
		t.Fatalf("解析 get_positions 失败: %v（原文 %s）", err, toolText(out))
	}
	for _, x := range p.Positions {
		if x.TsCode == code {
			return x.TotalQty
		}
	}
	return -1
}

// ============================================================================
// 3. 金额单位往返验证：元→分存储
// ============================================================================

func TestField_PriceUnitConversion(t *testing.T) {
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	t.Setenv("JZ_SERVER_API_TOKEN", "testtoken")

	st, err := store.Open(filepath.Join(t.TempDir(), "price_unit.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	defer st.Close()

	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	deps := Deps{
		Store:     st,
		Config:    cfg,
		Goal:      goal.NewService(st, goal.DefaultConfig(), ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000))),
		Freshness: dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), 0),
		Ledger:    ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000)),
		Tickets:   ticket.NewService(st),
	}
	srv, err := New(deps, "testtoken")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	ticketID, err := createDummyTicket(ctx, st, "20260901", "600519.SH", model.DirBuy, 1000)
	if err != nil {
		t.Fatalf("创建测试指令单失败：%v", err)
	}
	t.Logf("创建的指令单 ID: %d", ticketID)

	// 用 10.50 元买入
	priceYuan := 10.50
	res := mcpCallOK(t, ts, "report_fill", map[string]interface{}{
		"ticket_id": ticketID, "qty": 1000, "price": priceYuan,
	})

	// 验证返回的 fill 价格是否为分（应该是 1050 而不是 10.50）
	fillRaw, ok := res["fill"].(map[string]interface{})
	if !ok {
		t.Fatalf("report_fill 返回无 fill 字段：%v", res)
	}
	// Fen 类型在 JSON 中是 float64，但值应为整数；Go struct 字段大写
	priceFenRaw, ok := fillRaw["Price"]
	if !ok {
		t.Fatalf("fill.Price 不存在：%v", fillRaw)
	}
	switch p := priceFenRaw.(type) {
	case float64:
		expectedFen := float64(int(priceYuan * 100))
		if p != expectedFen {
			t.Errorf("report_fill 价格单位错误：传入 %.2f 元，数据库应存 %d 分，实际返回 %.2f 分",
				priceYuan, int(expectedFen), p)
		}
	case int64:
		expectedFen := int64(priceYuan * 100)
		if p != expectedFen {
			t.Errorf("report_fill 价格单位错误：传入 %.2f 元，数据库应存 %d 分，实际返回 %d 分",
				priceYuan, expectedFen, p)
		}
	default:
		t.Fatalf("fill.Price 类型不是 number: %T", priceFenRaw)
	}

	// 查库验证真实存储值（成交列就在指令单行上）
	tkB, err := st.TradeRepo().GetTicket(ctx, ticketID)
	if err != nil {
		t.Fatalf("回读指令单 %d 失败：%v", ticketID, err)
	}
	actualFill := tkB.FillView()
	if int64(actualFill.Price) != int64(model.FromFloat(priceYuan)) {
		t.Errorf("数据库存储价格错误：传入 %.2f 元，实际存了 %d 分（应为 %d 分）",
			priceYuan, int64(actualFill.Price), int64(model.FromFloat(priceYuan)))
	}
}

func TestField_SyncPortfolioUnits(t *testing.T) {
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	t.Setenv("JZ_SERVER_API_TOKEN", "testtoken")

	st, err := store.Open(filepath.Join(t.TempDir(), "sync_unit.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	defer st.Close()

	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	deps := Deps{
		Store:     st,
		Config:    cfg,
		Goal:      goal.NewService(st, goal.DefaultConfig(), ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000))),
		Freshness: dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), 0),
		Ledger:    ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000)),
		Tickets:   ticket.NewService(st),
	}
	srv, err := New(deps, "testtoken")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	costPriceYuan := 15.75
	initialCapitalYuan := 20000.0
	availableCashYuan := 14704.0

	mcpCallOK(t, ts, "sync_portfolio", map[string]interface{}{
		"date": "20260901", "initial_capital_yuan": initialCapitalYuan,
		"available_cash_yuan": availableCashYuan,
		"positions": []map[string]interface{}{
			{"ts_code": "600519.SH", "total_qty": 100, "cost_price": costPriceYuan},
		},
	})

	// 验证 position 成本价是否换算成分存库
	var pos model.Position
	err = st.WriteDB().GetContext(ctx, &pos, "SELECT * FROM position WHERE ts_code='600519.SH'")
	if err != nil {
		t.Fatalf("查询 position 表失败：%v", err)
	}
	expectedCostFen := int64(costPriceYuan * 100)
	if int64(pos.CostPrice) != expectedCostFen {
		t.Errorf("sync_portfolio 持仓成本单位错误：传入 %.2f 元，数据库存了 %d 分（应为 %d 分）",
			costPriceYuan, int64(pos.CostPrice), expectedCostFen)
	}

	// 验证 config_kv 中 account.initial_capital 是否以"元"存储（文档约定）
	raw, err := config.NewRepo(st).RawAll(ctx)
	if err != nil {
		t.Fatalf("读取 config_kv 失败：%v", err)
	}
	capitalVal, ok := raw["account.initial_capital"]
	if !ok || capitalVal == "" {
		t.Errorf("config_kv 缺少 account.initial_capital 键")
	} else if capitalVal != fmt.Sprintf("%.0f", initialCapitalYuan) {
		t.Errorf("sync_portfolio 本金单位错误：传入 %.2f 元，config_kv 存了 %q（应为 %.0f）",
			initialCapitalYuan, capitalVal, initialCapitalYuan)
	}

	// 验证 cash_anchor 是否以"分"存储
	anchorVal, ok := raw["account.cash_anchor"]
	if !ok || anchorVal == "" {
		t.Errorf("config_kv 缺少 account.cash_anchor 键")
	} else {
		anchorFen, err := strconv.ParseInt(anchorVal, 10, 64)
		if err != nil {
			t.Errorf("cash_anchor 解析失败：%v", err)
		} else {
			expectedAnchorFen := int64(availableCashYuan * 100)
			if anchorFen != expectedAnchorFen {
				t.Errorf("sync_portfolio 现金锚点单位错误：传入 %.2f 元，config_kv 存了 %d 分（应为 %d 分）",
					availableCashYuan, anchorFen, expectedAnchorFen)
			}
		}
	}
}

// ============================================================================
// 4. sync_portfolio 特定行为：缺 available_cash_yuan 必须失败
// ============================================================================

func TestField_SyncPortfolioMissingCash(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	// 只传 positions，不传 available_cash_yuan → 应该失败
	res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{
		"name": "sync_portfolio",
		"arguments": map[string]interface{}{
			"date": "20260901",
			"positions": []map[string]interface{}{
				{"ts_code": "600519.SH", "total_qty": 100, "cost_price": 15.0},
			},
		},
	})
	out, _ := res["result"].(map[string]interface{})
	if out == nil {
		t.Fatalf("sync_portfolio 无 result: %v", res)
	}
	if out["isError"] != true {
		t.Errorf("sync_portfolio 缺少 available_cash_yuan 应该失败，实际 isError=false, result=%v", out)
	} else {
		content, _ := out["content"].([]interface{})
		if len(content) > 0 {
			text, _ := content[0].(map[string]interface{})["text"].(string)
			t.Logf("正确拒绝：错误信息=%s", text)
		}
	}
}

// ============================================================================
// 5. sync_portfolio 返回体键名验证
// ============================================================================

func TestField_SyncPortfolioReturnKeys(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	// 首次同步
	res := mcpCallOK(t, ts, "sync_portfolio", map[string]interface{}{
		"date": "20260901", "initial_capital_yuan": 20000,
		"available_cash_yuan": 14704,
		"positions": []map[string]interface{}{
			{"ts_code": "600519.SH", "total_qty": 100, "cost_price": 15.0},
		},
	})

	// 验证返回体包含必需键
	requiredKeys := []string{"synced", "date", "cash_after_sync"}
	for _, k := range requiredKeys {
		if _, ok := res[k]; !ok {
			t.Errorf("sync_portfolio 返回体缺少必需键：%s", k)
		}
	}

	// 验证 cash_after_sync 的值与传入的 available_cash_yuan 一致
	cashStr, ok := res["cash_after_sync"].(string)
	if !ok {
		t.Errorf("cash_after_sync 类型不是 string: %T", res["cash_after_sync"])
	} else if !strings.Contains(cashStr, "14,704") {
		t.Errorf("cash_after_sync 回显错误：期望含 '14,704'，实际=%q", cashStr)
	}
}

// ============================================================================
// 6. sync_portfolio 本金 write-once：二次不同值拒绝覆盖
// ============================================================================

func TestField_SyncPortfolioCapitalRejected(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	// 首次同步
	first := mcpCallOK(t, ts, "sync_portfolio", map[string]interface{}{
		"date": "20260901", "initial_capital_yuan": 20000,
		"available_cash_yuan": 14704,
		"positions": []map[string]interface{}{
			{"ts_code": "600519.SH", "total_qty": 100, "cost_price": 15.0},
		},
	})

	if first["capital_rejected"] == true {
		t.Errorf("首次同步不应拒绝本金：%v", first)
	}

	// 第二次不同本金 → 应该拒绝
	second := mcpCallOK(t, ts, "sync_portfolio", map[string]interface{}{
		"date": "20260901", "initial_capital_yuan": 30000, // 不同值
		"available_cash_yuan": 20000,
		"positions": []map[string]interface{}{
			{"ts_code": "600519.SH", "total_qty": 100, "cost_price": 15.0},
		},
	})

	if second["capital_rejected"] != true {
		t.Errorf("第二次不同本金应被拒绝，实际 capital_rejected=%v", second["capital_rejected"])
	}
	if second["synced"] != float64(1) {
		t.Errorf("本金被拒时持仓仍应同步，实际 synced=%v", second["synced"])
	}
}

// ============================================================================
// 7. report_fill 幂等：同一 ticket_id 两次回执第二次返回 duplicate:true
// ============================================================================

func TestField_ReportFillIdempotent(t *testing.T) {
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	t.Setenv("JZ_SERVER_API_TOKEN", "testtoken")

	st, err := store.Open(filepath.Join(t.TempDir(), "idempotent.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	defer st.Close()

	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	deps := Deps{
		Store:     st,
		Config:    cfg,
		Goal:      goal.NewService(st, goal.DefaultConfig(), ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000))),
		Freshness: dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), 0),
		Ledger:    ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000)),
		Tickets:   ticket.NewService(st),
	}
	srv, err := New(deps, "testtoken")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()
	ticketID, err := createDummyTicket(ctx, st, "20260901", "600519.SH", model.DirBuy, 1000)
	if err != nil {
		t.Fatalf("创建测试指令单失败：%v", err)
	}

	// 第一次回执
	res1 := mcpCallOK(t, ts, "report_fill", map[string]interface{}{
		"ticket_id": ticketID, "qty": 1000, "price": 10.50,
	})
	if res1["duplicate"] == true {
		t.Errorf("首次回执不应标记为重复：%v", res1)
	}

	// 第二次相同回执 → 应该返回 duplicate:true
	res2 := mcpCallOK(t, ts, "report_fill", map[string]interface{}{
		"ticket_id": ticketID, "qty": 1000, "price": 10.50,
	})
	if res2["duplicate"] != true {
		t.Errorf("第二次回执应返回 duplicate:true，实际=%v", res2["duplicate"])
	}

	// 验证已登记回执数没有增加（成交回执就是指令单行的成交列）
	count, err := st.TradeRepo().CountFilled(ctx)
	if err != nil {
		t.Fatalf("统计回执数失败：%v", err)
	}
	if count != 1 {
		t.Errorf("幂等后应只有 1 笔记成回执，实际=%d", count)
	}
}

// ============================================================================
// 8. 读工具返回体键名与文档一致性
// ============================================================================

func TestField_ReadToolsReturnKeys(t *testing.T) {
	ts, cleanup := newTestServerForFields(t)
	defer cleanup()

	cases := []struct {
		name       string
		method     string
		params     map[string]interface{}
		wantKeys   []string
		notWantNil bool // 空结果不应是 null，应是 []
	}{
		// get_brief
		{"get_brief", "get_brief", map[string]interface{}{"date": "20260901"},
			[]string{"trade_date", "data_fresh", "blockers", "tickets_total", "tickets_pending", "positions"}, false},

		// get_tickets
		{"get_tickets", "get_tickets", map[string]interface{}{"date": "20260901"},
			[]string{"trade_date", "tickets"}, true},

		// get_positions
		{"get_positions", "get_positions", map[string]interface{}{},
			[]string{"positions"}, true},

		// get_portfolio
		{"get_portfolio", "get_portfolio", map[string]interface{}{},
			[]string{"trade_date", "cash_yuan", "market_value", "total_asset", "position_count"}, false},

		// get_logs
		{"get_logs", "get_logs", map[string]interface{}{"date": "20260901"},
			[]string{"trade_date", "trace"}, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{"name": c.method, "arguments": c.params})
			out, _ := res["result"].(map[string]interface{})
			if out == nil {
				t.Fatalf("%s 无 result: %v", c.name, res)
			}
			if out["isError"] == true {
				content, _ := out["content"].([]interface{})
				if len(content) > 0 {
					text, _ := content[0].(map[string]interface{})["text"].(string)
					t.Logf("%s 错误：%s", c.name, text)
				}
				return
			}

			// 解析 content[0].text 中的 JSON
			content, _ := out["content"].([]interface{})
			if len(content) == 0 {
				t.Fatalf("%s 无返回内容", c.name)
			}
			text, _ := content[0].(map[string]interface{})["text"].(string)
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(text), &payload); err != nil {
				t.Fatalf("%s 返回非 JSON 对象：%v (%s)", c.name, err, text)
			}

			// 验证必需键
			for _, k := range c.wantKeys {
				if _, ok := payload[k]; !ok {
					t.Errorf("%s 返回体缺少必需键：%s", c.name, k)
				}
			}

			// 验证数组不为 null
			if c.notWantNil {
				for _, k := range c.wantKeys {
					if v, ok := payload[k]; ok {
						if v == nil {
							t.Errorf("%s 键 %s 不应为 null，应返回空数组 []", c.name, k)
						}
					}
				}
			}
		})
	}
}

// ============================================================================
// 9. 写工具入参与返回体验证
// ============================================================================

func TestField_WriteToolsInputOutput(t *testing.T) {
	t.Setenv("JZ_TUSHARE_TOKEN", "dummy")
	t.Setenv("JZ_SERVER_API_TOKEN", "testtoken")

	st, err := store.Open(filepath.Join(t.TempDir(), "write_io.db"))
	if err != nil {
		t.Fatalf("打开测试库失败：%v", err)
	}
	defer st.Close()

	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		t.Fatalf("加载配置失败：%v", err)
	}
	deps := Deps{
		Store:     st,
		Config:    cfg,
		Goal:      goal.NewService(st, goal.DefaultConfig(), ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000))),
		Freshness: dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), 0),
		Ledger:    ticket.NewLedger(st, market.CostParams{}, model.FromFloat(10000)),
		Tickets:   ticket.NewService(st),
	}
	srv, err := New(deps, "testtoken")
	if err != nil {
		t.Fatalf("构造 MCP 服务失败：%v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx := context.Background()

	t.Run("skip_ticket reason 必填检验", func(t *testing.T) {
		// 先创建一张 drafted 状态的指令单
		ticketID, err := createDummyTicket(ctx, st, "20260901", "600519.SH", model.DirBuy, 1000)
		if err != nil {
			t.Fatalf("创建测试指令单失败：%v", err)
		}

		// 缺 reason → 拒绝
		res := mcpCall(t, ts, "testtoken", "tools/call", map[string]interface{}{
			"name": "skip_ticket",
			"arguments": map[string]interface{}{
				"ticket_id": ticketID,
			},
		})
		out, _ := res["result"].(map[string]interface{})
		if out == nil || out["isError"] != true {
			t.Errorf("skip_ticket 缺 reason 应拒绝，实际=%v", out)
		}
	})

	t.Run("set_gear 返回 gear 字段", func(t *testing.T) {
		res := mcpCallOK(t, ts, "set_gear", map[string]interface{}{
			"gear": "G1", "reason": "test_reason",
		})
		// set_gear 返回复杂结构，包含 Decision 字段
		decision, ok := res["Decision"].(map[string]interface{})
		if !ok {
			t.Fatalf("set_gear 返回无 Decision 字段：%v", res)
		}
		gear, ok := decision["To"].(string)
		if !ok || gear != "G1" {
			t.Errorf("set_gear 返回体缺少正确 gear 字段 (To)，实际=%v", decision)
		}
	})

	t.Run("confirm_pace 返回 confirmed", func(t *testing.T) {
		res := mcpCallOK(t, ts, "confirm_pace", map[string]interface{}{
			"date": "20260901",
		})
		if res["confirmed"] != true {
			t.Errorf("confirm_pace 应返回 confirmed=true，实际=%v", res)
		}
	})
}

// ============================================================================
// 辅助函数
// ============================================================================

func createDummyTicket(ctx context.Context, st *store.Store, date, tsCode string, dir model.Direction, qty int64) (int64, error) {
	// 走仓储的写入口，不在测试里另抄一份列清单 —— 列变了这里必须跟着变，那正是想要的信号。
	return st.TradeRepo().InsertTicket(ctx, model.OrderTicket{
		TradeDate: date, TsCode: tsCode, Name: "测试股", Direction: dir,
		Qty: model.Qty(qty), RefPrice: model.FromFloat(10.5), Reason: "测试原因",
		Status: model.TicketDrafted, ValidUntil: "2026-09-02T15:00:00+08:00", Gear: model.GearG1,
	})
}

// ============================================================================
// 测试入口
// ============================================================================

func TestField(t *testing.T) {
	t.Run("MissingRequiredParams", TestField_MissingRequiredParams)
	t.Run("InvalidEnumAndFormat", TestField_InvalidEnumAndFormat)
	t.Run("PriceUnitConversion", TestField_PriceUnitConversion)
	t.Run("SyncPortfolioUnits", TestField_SyncPortfolioUnits)
	t.Run("SyncPortfolioMissingCash", TestField_SyncPortfolioMissingCash)
	t.Run("SyncPortfolioReturnKeys", TestField_SyncPortfolioReturnKeys)
	t.Run("SyncPortfolioCapitalRejected", TestField_SyncPortfolioCapitalRejected)
	t.Run("ReportFillIdempotent", TestField_ReportFillIdempotent)
	t.Run("ReadToolsReturnKeys", TestField_ReadToolsReturnKeys)
	t.Run("WriteToolsInputOutput", TestField_WriteToolsInputOutput)
}
