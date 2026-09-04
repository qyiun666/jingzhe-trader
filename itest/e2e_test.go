package itest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/mcp"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// TestTradingDayEndToEnd 一个完整交易日的真实闭环：
// 同步 → 门禁 → 选股 → LLM 决策 → 指令单 → 四类邮件 + 盘中紧急告警 → 组合同步 → 定目标 → MCP 对外接口。
//
// 这是唯一能回答"每天到底能不能正常跑"的测试：单元测试全用桩，
// 而历史上出问题的一直是桩覆盖不到的接缝（邮件配置缺失、指数缺失、行情降级）。
// 子测试按时间顺序跑，共享同一个运行时与同一个交易日锚点。
func TestTradingDayEndToEnd(t *testing.T) {
	rt, _ := requireRuntime(t)
	ctx := t.Context()
	date := tradeDateOr(t, "20260903")
	st := rt.Store

	// ---------- ① 每日闭环：同步 → 门禁 → 档位 → 选股漏斗 ----------
	started := time.Now()
	mustNoErr(t, "evening_pipeline", rt.RunTaskOnce(ctx, "evening_pipeline", date, "itest"))
	outcome, detail := jobOutcome(t, st, date, "evening_pipeline")
	if outcome == model.TraceFail {
		t.Fatalf("流水线失败: %s", detail)
	}
	bars, err := st.MarketRepo().CountBar(ctx, date)
	mustNoErr(t, "CountBar", err)
	if bars < 5000 {
		t.Fatalf("当日日线只落了 %d 行，全市场同步没成功", bars)
	}
	if n, _ := st.MarketRepo().CountIndexBar(ctx, store.MarketIndex, date); n == 0 {
		t.Fatalf("%s 指数日线未落库，买入闸门无输入", store.MarketIndex)
	}
	if n, _ := st.MarketRepo().CountValuation(ctx, date); n < 5000 {
		t.Fatalf("当日估值截面只有 %d 行", n)
	}
	// 沪深300 在 MA20 下方时闸门关漏斗，候选 0 是正确结果（partial + SCREEN_EMPTY），不是故障。
	if outcome == model.TracePartial && !strings.Contains(detail, "SCREEN_EMPTY") {
		t.Errorf("流水线带着非 SCREEN_EMPTY 的降级完成: %s", detail)
	}
	t.Log(describe("流水线", outcome, "耗时秒", time.Since(started).Seconds(),
		"日线", bars, "估值", mustCount(st, ctx, date)))

	// ---------- ①b 决策链：选股 → LLM 买入决策 → 指令单 ----------
	// 大盘闸门按当天真实方向走，所以这里用同一套漏斗、只把 MarketOK 拨成允许，
	// 证明"选出候选以后 LLM 真的被问过、指令单真的写得进去"。
	cand, err := rt.Screener.Run(ctx, date, screener.Budget{
		Cash: model.FromFloat(20000), Slots: 2, MarketOK: true})
	mustNoErr(t, "Screener.Run", err)
	if len(cand.Candidates) == 0 {
		t.Fatalf("放开大盘闸门后漏斗仍然 0 候选，漏斗各级: %s", stageText(cand))
	}
	// 整批都问：截断正是在"一批 6 只 + 长理由"时才出现，砍到 3 只就测不到了。
	ask := cand.Candidates
	rp0, gear0, err := rt.Goal.RiskParams(ctx, date)
	mustNoErr(t, "RiskParams", err)
	// 生效风控参数不许带零值：零值止损＝成本价即止损线，会把每个持仓都判成该清仓。
	// 这一串断言防的是"档位状态读出来是空的但没人报错"这类静默口径失效。
	if rp0.StopLossPct <= 0 || rp0.StopLossPct >= 1 {
		t.Fatalf("%s 档生效止损线=%.3f，不在 (0,1) 内", gear0, rp0.StopLossPct)
	}
	if rp0.TrailingStopPct <= 0 || rp0.MaxPositions <= 0 || rp0.MaxTotalPositionPct <= 0 {
		t.Fatalf("%s 档生效风控参数疑似读空: 移动止盈=%.3f 最大持仓=%d 总仓上限=%.3f",
			gear0, rp0.TrailingStopPct, rp0.MaxPositions, rp0.MaxTotalPositionPct)
	}
	drep, err := rt.Signal.Generate(ctx, date, ask, rp0, gear0, rt.Decider)
	mustNoErr(t, "Signal.Generate", err)
	if drep.Approved+drep.Declined+drep.Failed != len(ask) {
		t.Errorf("LLM 裁决没有覆盖整批候选: 批准%d 否决%d 失败%d，问了 %d 只",
			drep.Approved, drep.Declined, drep.Failed, len(ask))
	}
	if drep.Failed > 0 {
		// 把失败行的 detail 直接打出来：只报"未问出结果"等于把问题留给下一个人重新查。
		rows, lerr := st.TraceRepo().List(ctx, date)
		mustNoErr(t, "读轨迹", lerr)
		for _, r := range rows {
			if strings.HasPrefix(r.Subject, "llm:") && r.Outcome == model.TraceFail {
				t.Logf("LLM 失败行 %s: %.300s", r.Subject, r.Detail)
			}
		}
		t.Errorf("%d 只候选评审未问出结果", drep.Failed)
	}
	if n := countTraces(t, st, date, func(s string) bool { return strings.HasPrefix(s, "llm:") }); n == 0 {
		t.Error("没有任何 llm:* 轨迹行 —— 买入决策这一步根本没跑")
	} else {
		t.Log(describe("候选", len(cand.Candidates), "问模型", len(ask), "批准", drep.Approved,
			"否决", drep.Declined, "新增指令单", drep.Tickets, "LLM轨迹", n))
	}

	// ---------- ② 更新持仓（券商口径校准）----------
	const hold = "600519.SH"
	qs, err := quote.NewGotdxSource().Fetch(ctx, []string{hold})
	mustNoErr(t, "取 "+hold+" 实时价", err)
	marketPrice := qs[hold].Price
	if marketPrice <= 0 {
		t.Fatalf("%s 实时价非正: %d", hold, marketPrice)
	}
	t.Log(describe(hold, "现价", marketPrice.Float()))

	synced, rejected, err := rt.Ledger.SyncPortfolio(ctx, ticket.PortfolioSync{
		Date: date, Capital: model.FromFloat(20000), Cash: model.FromFloat(2000),
		Items: []ticket.PortfolioInput{{
			TsCode: hold, TotalQty: 100, AvailableQty: 100,
			CostPrice: marketPrice * 3, HighPrice: marketPrice * 3,
		}},
		Actor: "itest",
	})
	mustNoErr(t, "SyncPortfolio", err)
	if synced != 1 || rejected {
		t.Fatalf("组合同步结果异常: synced=%d rejected=%v", synced, rejected)
	}
	pos, err := st.TradeRepo().GetPosition(ctx, hold)
	mustNoErr(t, "GetPosition", err)
	if pos.TotalQty != 100 || pos.Available() != 100 {
		t.Errorf("持仓校准不对: %+v", pos)
	}
	ast, err := rt.Ledger.Assets(ctx, date)
	mustNoErr(t, "Assets", err)
	if ast.Cash <= 0 {
		t.Errorf("现金口径为 %d，锚点没落地", int64(ast.Cash))
	}
	t.Log(describe("持仓行", synced, "现金", int64(ast.Cash), "总资产", int64(ast.TotalAsset)))

	// ---------- ③ 定目标：季度评估 + 人工改档 + 档位真的改变风控口径 ----------
	res, err := rt.Goal.Evaluate(ctx, date)
	mustNoErr(t, "goal.Evaluate", err)
	t.Log(describe("档位", string(res.Decision.To), "进度", fmt.Sprintf("%.2f%%", res.Metrics.Progress*100)))

	res2, err := rt.Goal.SetGear(ctx, model.GearG3, "集成测试改档", "20991231", "itest")
	if err != nil {
		t.Fatalf("SetGear: %v", err)
	}
	// 改档回包里的度量必须是真数：曾经这里返回一串零，agent 会把"没算"读成"目标 0%"。
	if res2.Metrics.TargetPct <= 0 || res2.Metrics.TotalDays <= 0 || res2.Metrics.CurrentAsset <= 0 {
		t.Errorf("set_gear 回包的度量为零值: %+v", res2.Metrics)
	}
	rp, gear, err := rt.Goal.RiskParams(ctx, date)
	mustNoErr(t, "RiskParams", err)
	if gear != model.GearG3 {
		t.Fatalf("改档后生效档位是 %s，期望 G3", gear)
	}
	if rp.AllowNewPosition {
		t.Error("G3 防守档仍允许开新仓，档位状态机没生效")
	}
	if rp.StopLossPct <= 0 {
		t.Errorf("G3 止损线为 %v，风控参数读空", rp.StopLossPct)
	}

	// ---------- ④ 盘中紧急：持仓深亏 → 必出止损单 + M3 邮件 ----------
	// 成本价刻意设为现价 3 倍，止损线远高于现价，扫描必然触发。
	mustNoErr(t, "intraday_scan", rt.RunTaskOnce(ctx, "intraday_scan", date, "itest"))
	assertJobOutcome(t, st, date, "intraday_scan", model.TraceOK)
	sells, err := st.TradeRepo().ListActiveTickets(ctx, date)
	mustNoErr(t, "ListActiveTickets", err)
	var stopSell *model.OrderTicket
	for i := range sells {
		if sells[i].TsCode == hold && sells[i].Direction == model.DirSell {
			stopSell = &sells[i]
		}
	}
	if stopSell == nil {
		t.Fatal("深亏持仓没有产生止损卖单（盘中盯盘的核心职责）")
	}
	if got := countTraces(t, st, date, func(s string) bool { return s == model.TraceMail(model.MailM3) }); got == 0 {
		t.Fatal("止损卖单已建但没有 mail:M3 轨迹 —— 紧急邮件没发")
	} else if !lastTraceOK(t, st, date, model.TraceMail(model.MailM3)) {
		t.Fatal("mail:M3 轨迹是 fail，紧急邮件实际没投递成功")
	}
	t.Log(describe("止损单", stopSell.ID, "数量", int64(stopSell.Qty), "理由", stopSell.Reason))

	// ---------- ⑤ 四类邮件真的投递：M2 盘前 / M1 指令 / M5 日报 ----------
	for _, tc := range []struct {
		job, mail string
		typ       model.MailType
	}{
		{"morning_plan", "M2", model.MailM2},
		{"mail_pending", "M1", model.MailM1},
		{"daily_report", "M5", model.MailM5},
	} {
		if err := rt.RunTaskOnce(ctx, tc.job, date, "itest"); err != nil {
			t.Errorf("%s 失败: %v", tc.job, err)
			continue
		}
		if !lastTraceOK(t, st, date, model.TraceMail(tc.typ)) {
			t.Errorf("%s 跑完了但 %s 邮件没有成功投递记录", tc.job, tc.mail)
		}
	}
	// 日报里的任务分列必须看得见今天这一整串（成功/降级/失败三列）。
	if n := countTraces(t, st, date, func(s string) bool { return strings.HasPrefix(s, "job:") }); n < 4 {
		t.Errorf("当日只有 %d 行 job: 轨迹，少于本测试跑过的 4 个任务", n)
	}

	// ---------- ⑥ MCP 对外接口：真实运行时 + 真实数据，走 HTTP ----------
	token := "itest-token-0123456789abcdef"
	if _, err := mcp.New(rt.MCPDeps(), ""); err == nil {
		t.Error("空令牌的 MCP 服务竟然构造成功：serve 会带着无鉴权接口上线")
	}
	srv, err := mcp.New(rt.MCPDeps(), token)
	if err != nil {
		t.Fatalf("构造 MCP 服务失败: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, name := range []string{"get_brief", "get_tickets", "get_positions", "get_portfolio", "get_logs"} {
		out := callTool(t, ts, token, name, map[string]interface{}{"date": date})
		if out.isError {
			t.Errorf("%s 在真实数据上失败: %s", name, out.text)
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal([]byte(out.text), &m) != nil {
			t.Errorf("%s 返回的不是 JSON 对象: %.120s", name, out.text)
			continue
		}
		if arr, ok := m["tickets"].([]interface{}); ok && name == "get_tickets" && len(arr) == 0 {
			t.Errorf("get_tickets 在已建止损单之后返回空列表")
		}
		if pos, ok := m["positions"].([]interface{}); ok && name == "get_positions" && len(pos) == 0 {
			t.Errorf("get_positions 在已同步持仓之后返回空列表")
		}
		t.Log(describe("MCP", name, "字节", len(out.text)))
	}

	// 未知任务名必须被拒绝：README 里曾经列着 calendar/daily/freshness/screen
	// 这些早已不存在的任务，agent 照文档调用就会拿到一个含义不明的失败。
	if out := callTool(t, ts, token, "trigger_task",
		map[string]interface{}{"task": "freshness", "date": date}); !out.isError {
		t.Errorf("trigger_task 接受了不存在的任务 freshness，返回: %s", out.text)
	}
	if out := callTool(t, ts, token, "trigger_task",
		map[string]interface{}{"task": "不存在的任务", "date": date}); !out.isError {
		t.Errorf("trigger_task 对未知任务没有报错: %s", out.text)
	}

	// 日期格式在服务端分发前就要拒掉：下游 market.QuarterOf 按 date[:4] 定长切片，
	// "2026" 这种短串会先在纯函数里 panic，再被调度器 recover 成一条看不出所以然的失败。
	for _, bad := range []string{"2026", "2026-09-03", "20260230", "abc"} {
		if out := callTool(t, ts, token, "trigger_task",
			map[string]interface{}{"task": "daily_report", "date": bad}); !out.isError {
			t.Errorf("trigger_task 接受了非法日期 %q，返回: %.160s", bad, out.text)
		} else if !strings.Contains(out.text, "YYYYMMDD") {
			t.Errorf("非法日期 %q 的拒绝信息里没说清格式要求: %s", bad, out.text)
		}
		if out := callTool(t, ts, token, "get_tickets",
			map[string]interface{}{"date": bad}); !out.isError {
			t.Errorf("get_tickets 接受了非法日期 %q，会把它当成「当天没有单」回给 agent: %.160s", bad, out.text)
		}
	}
}

// ===================== 断言辅助 =====================

type tracePred func(subject string) bool

func countTraces(t *testing.T, st *store.Store, date string, want tracePred) int {
	t.Helper()
	rows, err := st.TraceRepo().List(t.Context(), date)
	mustNoErr(t, "读轨迹", err)
	n := 0
	for _, r := range rows {
		if want(r.Subject) {
			n++
		}
	}
	return n
}

func lastTraceOK(t *testing.T, st *store.Store, date, subject string) bool {
	t.Helper()
	rows, err := st.TraceRepo().List(t.Context(), date)
	mustNoErr(t, "读轨迹", err)
	found := false
	ok := true
	for _, r := range rows {
		if r.Subject == subject {
			found = true
			ok = r.Outcome == model.TraceOK
		}
	}
	if !found {
		t.Logf("轨迹里没有 %s 这一行", subject)
	}
	return found && ok
}

// stageText 把漏斗各级的存量拼成一行（0 候选时必须能看出卡在哪一级）。
func stageText(r *screener.Report) string {
	parts := make([]string, 0, len(r.Stages))
	for _, st := range r.Stages {
		parts = append(parts, fmt.Sprintf("%s→%d", st.Slug, st.Out))
	}
	return strings.Join(parts, " ")
}

// jobOutcome 取某任务当日最后一条轨迹（outcome + detail）。
func jobOutcome(t *testing.T, st *store.Store, date, job string) (string, string) {
	t.Helper()
	subject := model.TraceJob(job)
	rows, err := st.TraceRepo().List(t.Context(), date)
	mustNoErr(t, "读轨迹", err)
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Subject == subject {
			return rows[i].Outcome, rows[i].Detail
		}
	}
	t.Fatalf("任务 %s 没有留下任何轨迹行（runJob 未执行或写库失败）", job)
	return "", ""
}

func assertJobOutcome(t *testing.T, st *store.Store, date, job string, want string) {
	t.Helper()
	subject := model.TraceJob(job)
	rows, err := st.TraceRepo().List(t.Context(), date)
	mustNoErr(t, "读轨迹", err)
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Subject != subject {
			continue
		}
		if rows[i].Outcome != want {
			t.Fatalf("任务 %s outcome=%q（期望 %q），detail=%.300s",
				job, rows[i].Outcome, want, rows[i].Detail)
		}
		return
	}
	t.Fatalf("任务 %s 没有留下任何轨迹行（runJob 未执行或写库失败）", job)
}

func mustCount(st *store.Store, ctx context.Context, date string) int {
	n, err := st.MarketRepo().CountValuation(ctx, date)
	if err != nil {
		return -1
	}
	return n
}

// ===================== MCP 客户端辅助 =====================

type toolOut struct {
	isError bool
	text    string
}

func callTool(t *testing.T, ts *httptest.Server, token, name string, args map[string]interface{}) toolOut {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]interface{}{"name": name, "arguments": args},
	})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(string(body)))
	mustNoErr(t, "构造 MCP 请求", err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	mustNoErr(t, "调用 MCP", err)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MCP %s 返回 http %d", name, resp.StatusCode)
	}
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	dec := json.NewDecoder(resp.Body)
	mustNoErr(t, "解析 MCP 响应", dec.Decode(&envelope))
	if envelope.Error != nil {
		return toolOut{isError: true, text: envelope.Error.Message}
	}
	var sb strings.Builder
	for _, c := range envelope.Result.Content {
		sb.WriteString(c.Text)
	}
	return toolOut{isError: envelope.Result.IsError, text: sb.String()}
}
