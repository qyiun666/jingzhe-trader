package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
)

// ===================== 测试脚手架 =====================

// 分发标记必须取各系统提示词里独一无二的片段：决策提示词正文里也会出现
// "技术形态""消息面"这些词，用裸维度名分发会串到证据分支上（本文件第一版就踩了）。
const (
	markTech   = "本次维度：技术形态"
	markValue  = "本次维度：基本面"
	markNews   = "本次维度：消息面"
	markSector = "本次维度：板块地位"
	markDecide = "的买入决策者"

	promptsPerRun = 5 // 4 条证据 + 1 条决策：整批只问一次，与候选数量无关
	codeA         = "600001.SH"
	codeB         = "600002.SH"
)

// stubNoMessage 用作回复占位：模拟模型 status=completed 却只吐 reasoning 就收尾。
const stubNoMessage = "<<NO-MESSAGE>>"

type stubCall struct {
	marker string
	size   string // web_search 的 search_context_size；未挂工具时为空
	user   string // 本次请求的正文（断言"只问未答过的标的"要看它）
}

// stubServer 按系统提示词片段分发回复：每个标记可以给一串回复，
// 第 N 次问到它就返回第 N 条（用尽后重复最后一条）。匹配不上直接失败，防止悄悄走默认分支。
type stubServer struct {
	t       *testing.T
	srv     *httptest.Server
	replies map[string][]string
	fails   map[string]int // 标记 → 还剩多少次返回 500（nil = 从不失败）

	mu    sync.Mutex
	calls []stubCall
}

func newStub(t *testing.T, replies map[string][]string, failTimes map[string]int) *stubServer {
	t.Helper()
	s := &stubServer{t: t, replies: replies, fails: failTimes}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req responseReq
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求体不是合法 JSON: %v", err)
		}
		marker := matchMarker(req.Instructions)
		if marker == "" {
			t.Errorf("stub 未匹配到任何维度，系统提示词片段：%s", clip(req.Instructions, 80))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		asked := len(s.callsOfLocked(marker))
		s.calls = append(s.calls, stubCall{marker: marker, size: searchSizeOf(req.Tools), user: req.Input})
		failLeft := s.fails[marker]
		if failLeft > 0 {
			s.fails[marker] = failLeft - 1
		}
		list := s.replies[marker]
		reply := list[len(list)-1]
		if asked < len(list) {
			reply = list[asked]
		}
		s.mu.Unlock()

		if failLeft > 0 {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"error":{"message":"上游 500"}}`)
			return
		}
		fmt.Fprintf(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"text","text":%q}]}]}`, reply)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// matchMarker 找到这条请求属于哪条 prompt。
func matchMarker(instructions string) string {
	for _, m := range []string{markTech, markValue, markNews, markSector, markDecide} {
		if strings.Contains(instructions, m) {
			return m
		}
	}
	return ""
}

// searchSizeOf 请求里挂的检索档；没挂工具返回空串。
func searchSizeOf(tools []toolSpec) string {
	for _, tl := range tools {
		if tl.Type == "web_search" {
			return tl.SearchContextSize
		}
	}
	return ""
}

func (s *stubServer) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// callsOf 某条 prompt 一共被问了几次（重试次数就靠它断言）。
func (s *stubServer) callsOf(marker string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.callsOfLocked(marker))
}

func (s *stubServer) callsOfLocked(marker string) []stubCall {
	var out []stubCall
	for _, c := range s.calls {
		if c.marker == marker {
			out = append(out, c)
		}
	}
	return out
}

func (s *stubServer) of(marker string) []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callsOfLocked(marker)
}

func (s *stubServer) sizesOf(marker string) []string {
	var out []string
	for _, c := range s.of(marker) {
		out = append(out, c.size)
	}
	return out
}

func (s *stubServer) usersOf(marker string) []string {
	var out []string
	for _, c := range s.of(marker) {
		out = append(out, c.user)
	}
	return out
}

// allOKBatch 两个标的都有结论、决策给出不同仓位的回复集。
func allOKBatch() map[string][]string {
	return map[string][]string{
		markTech:   {evResults(codeA, "positive", "MA5 上穿 MA20 且量能配合", codeB, "neutral", "横盘无量")},
		markValue:  {evResults(codeA, "neutral", "PE 12 倍处中性区间", codeB, "unknown", "估值数据不足")},
		markNews:   {evResults(codeA, "unknown", "无消息面数据，不作判断", codeB, "neutral", "检索未见风险公告")},
		markSector: {evResults(codeA, "positive", "板块内领涨且冲击成本可忽略", codeB, "negative", "投入占日成交额 8%")},
		markDecide: {decResults(codeA, true, 0.3, 0.72, "技术面与板块地位均为 positive",
			codeB, false, 0, 0.2, "板块维度 negative，不买")},
	}
}

// onlyCodeC 新增一只标的时的回复集（四条证据 + 决策都只覆盖它）。
func onlyCodeC(codeC string) map[string][]string {
	return map[string][]string{
		markTech:   {evResults(codeC, "positive", "形态完好")},
		markValue:  {evResults(codeC, "neutral", "估值中性")},
		markNews:   {evResults(codeC, "neutral", "检索未见风险公告")},
		markSector: {evResults(codeC, "positive", "冲击成本低")},
		markDecide: {decResults(codeC, true, 0.15, 0.6, "三条 positive 且资金够")},
	}
}

// evResults 拼证据批量回复：每三个参数一组（代码、stance、finding）。
func evResults(spec ...string) string {
	var b strings.Builder
	b.WriteString(`{"results":[`)
	for i := 0; i+2 < len(spec); i += 3 {
		fmt.Fprintf(&b, `{"ts_code":%q,"stance":%q,"finding":%q,"unknown":false},`,
			spec[i], spec[i+1], spec[i+2])
	}
	return strings.TrimSuffix(b.String(), ",") + "]}"
}

// decResults 拼决策批量回复：每组五个参数（代码、approve、weight、confidence、reason）。
func decResults(spec ...any) string {
	var b strings.Builder
	b.WriteString(`{"results":[`)
	for i := 0; i+4 < len(spec); i += 5 {
		fmt.Fprintf(&b, `{"ts_code":%q,"approve":%v,"weight_pct":%v,"confidence":%v,"reason":%q},`,
			spec[i], spec[i+1], spec[i+2], spec[i+3], spec[i+4])
	}
	return strings.TrimSuffix(b.String(), ",") + "]}"
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/llm.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newReviewer(url string, st *store.Store, enabled bool) *Reviewer {
	r := NewReviewer(NewClient("test-key", url, "test-model", "high", nil), st, enabled)
	// 退避基数压到 1ms：重试语义照常验证，但单测不必真的等 2s/4s。
	return r.WithRetryBackoff(time.Millisecond)
}

func item(code, name string) signal.BuyRequest {
	closes := make([]float64, 25)
	vols := make([]float64, 25)
	raws := make([]float64, 25)
	for i := range closes {
		closes[i] = 10 + float64(i)*0.1
		vols[i] = 100
		raws[i] = closes[i] * 100
	}
	return signal.BuyRequest{
		TradeDate: "20260903",
		Candidate: model.Candidate{TsCode: code, Name: name, Industry: "半导体",
			Score: 71.3, Close: model.FromFloat(12.3), CircMvW: 560000, PETtm: 12.4, PB: 2.1,
			TurnoverRate: 2.3, Mom: 0.08, SectorMom: 0.05, Reason: "综合分靠前", PoolSize: 6},
		Bars:    signal.BarSeries{Closes: closes, Vols: vols, Raws: raws},
		RulesOK: true,
		Budget: signal.BuyBudget{CashFen: model.FromFloat(20000), SlotFen: model.FromFloat(8000),
			LotCostFen: model.FromFloat(1230), Positions: 1, MaxPos: 2},
	}
}

func batchReq(items ...signal.BuyRequest) signal.BatchRequest {
	if len(items) == 0 {
		items = []signal.BuyRequest{item(codeA, "甲"), item(codeB, "乙")}
	}
	return signal.BatchRequest{TradeDate: "20260903", Items: items}
}

// rowOf 取某标某条 prompt 的记录。
func rowOf(t *testing.T, st *store.Store, code, key string) (model.LLMCall, bool) {
	t.Helper()
	c, found, err := st.LLMRepo().GetCall(context.Background(), "20260903", code, key)
	if err != nil {
		t.Fatalf("读取 LLM 留痕失败: %v", err)
	}
	return c, found
}

func mustRow(t *testing.T, st *store.Store, code, key string) model.LLMCall {
	t.Helper()
	c, found := rowOf(t, st, code, key)
	if !found {
		t.Fatalf("缺少 %s/%s 记录", code, key)
	}
	return c
}

// ===================== 用例 =====================

// TestBatchAsksOncePerPrompt 是这轮改动的核心断言：候选数量不进调用次数。
// 两只票仍然只有 5 次 HTTP 调用（4 证据 + 1 决策），但每只票各留 5 行结论。
func TestBatchAsksOncePerPrompt(t *testing.T) {
	st := testStore(t)
	srv := newStub(t, allOKBatch(), nil)
	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatalf("DecideBatch 失败: %v", err)
	}
	if srv.total() != promptsPerRun {
		t.Errorf("整批 2 只应只发 %d 次请求，实际 %d", promptsPerRun, srv.total())
	}
	a, b := got[codeA], got[codeB]
	if !a.Approve || a.WeightPct != 0.3 || a.Confidence != 0.72 {
		t.Errorf("甲裁决不符: %+v", a)
	}
	if !strings.Contains(a.Reason, "30%") {
		t.Errorf("reason 应写明比例便于邮件复核: %q", a.Reason)
	}
	if b.Approve || b.Failed {
		t.Errorf("乙应被模型否决（不是评审失败）: %+v", b)
	}
	for _, code := range []string{codeA, codeB} {
		for _, key := range []string{KeyTech, KeyValue, KeyNews, KeySector, KeyDecision} {
			if mustRow(t, st, code, key).Status != model.TraceOK {
				t.Errorf("%s/%s 状态应为 ok", code, key)
			}
		}
	}
	if v := mustRow(t, st, codeA, KeyNews).Verdict; v != stanceUnknown {
		t.Errorf("消息面答 unknown 必须原样落库，实际 %q", v)
	}
	if v := mustRow(t, st, codeA, KeyDecision).Verdict; v != verdictBuy {
		t.Errorf("决策行 verdict=%q，期望 %q", v, verdictBuy)
	}
	if v := mustRow(t, st, codeB, KeyDecision).Verdict; v != verdictSkip {
		t.Errorf("被否决的决策行 verdict=%q，期望 %q", v, verdictSkip)
	}
}

// TestBatchMessageCarriesEveryCode 一次调用必须把每只票的数据都给出去，
// 并要求逐只照抄 ts_code —— 否则模型只能猜。
func TestBatchMessageCarriesEveryCode(t *testing.T) {
	st := testStore(t)
	srv := newStub(t, allOKBatch(), nil)
	if _, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq()); err != nil {
		t.Fatal(err)
	}
	users := srv.usersOf(markTech)
	if len(users) != 1 {
		t.Fatalf("技术形态应只问一次，实际 %d", len(users))
	}
	for _, want := range []string{codeA, codeB, "甲", "乙", `"results"`} {
		if !strings.Contains(users[0], want) {
			t.Errorf("批量正文缺少 %q", want)
		}
	}
	if n := strings.Count(users[0], "——— 第"); n != 2 {
		t.Errorf("应分成 2 段标的，实际 %d 段", n)
	}
}

// TestReviewRerunHitsCache 当日重跑不得再烧一次 token：五条 prompt 全部命中缓存，调用数不变。
func TestReviewRerunHitsCache(t *testing.T) {
	st := testStore(t)
	srv := newStub(t, allOKBatch(), nil)
	r := newReviewer(srv.srv.URL, st, true)

	for i := 0; i < 3; i++ {
		got, err := r.DecideBatch(context.Background(), batchReq())
		if err != nil {
			t.Fatalf("第 %d 次重跑失败: %v", i+1, err)
		}
		// 缓存必须还原出完整的裁决（含仓位比例）：只缓存"买"而不缓存"买多少"，
		// 第二轮就会把它判成"批准但没给仓位 = 不买"，与首轮结果不一致。
		if a := got[codeA]; !a.Approve || a.WeightPct != 0.3 {
			t.Errorf("第 %d 次重跑裁决走样: %+v", i+1, a)
		}
		if b := got[codeB]; b.Approve {
			t.Errorf("第 %d 次重跑把否决的票翻案了: %+v", i+1, b)
		}
	}
	if srv.total() != promptsPerRun {
		t.Errorf("重跑 3 次仍应只调用 %d 次，实际 %d", promptsPerRun, srv.total())
	}
}

// TestRerunOnlyAsksNewCode 缓存命中按标的算：新增一只票时，
// 四条 prompt 只问新来的那只，已答过的两只不再进正文（不白烧 token）。
func TestRerunOnlyAsksNewCode(t *testing.T) {
	st := testStore(t)
	first := newStub(t, allOKBatch(), nil)
	if _, err := newReviewer(first.srv.URL, st, true).DecideBatch(context.Background(), batchReq()); err != nil {
		t.Fatal(err)
	}

	codeC := "600003.SH"
	second := newStub(t, onlyCodeC(codeC), nil)
	three := batchReq(item(codeA, "甲"), item(codeB, "乙"), item(codeC, "丙"))
	got, err := newReviewer(second.srv.URL, st, true).DecideBatch(context.Background(), three)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("三只都应有结论，实际 %d", len(got))
	}
	if c := got[codeC]; !c.Approve || c.WeightPct != 0.15 {
		t.Errorf("新标的丙应被批准 15%%: %+v", c)
	}
	if a := got[codeA]; !a.Approve || a.WeightPct != 0.3 {
		t.Errorf("已缓存的甲应复用首轮结论: %+v", a)
	}
	if second.total() != promptsPerRun {
		t.Errorf("只有丙需要重问，调用数应为 %d，实际 %d", promptsPerRun, second.total())
	}
	for _, m := range []string{markTech, markDecide} {
		users := second.usersOf(m)
		if len(users) != 1 {
			t.Fatalf("%s 应只问一次，实际 %d", m, len(users))
		}
		if strings.Contains(users[0], codeA) || strings.Contains(users[0], codeB) {
			t.Errorf("%s 的正文里混进了已缓存的标的，白烧 token", m)
		}
		if !strings.Contains(users[0], codeC) {
			t.Errorf("%s 的正文必须包含待问的 %s", m, codeC)
		}
	}
}

// TestPartialBatchReply 模型漏答某只标的：那只记 Failed，答到的照常出决策。
// 缺一行不应当拖垮整批 —— 那等于让一次输出抖动变成当日零计划。
func TestPartialBatchReply(t *testing.T) {
	st := testStore(t)
	replies := allOKBatch()
	replies[markNews] = []string{evResults(codeA, "neutral", "检索未见风险公告")} // 乙漏答
	srv := newStub(t, replies, nil)
	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatalf("漏答不应中断流程: %v", err)
	}
	if !got[codeA].Approve {
		t.Errorf("答全的甲应照常出决策: %+v", got[codeA])
	}
	if !got[codeB].Failed || got[codeB].Approve {
		t.Errorf("漏答的乙应记为评审失败: %+v", got[codeB])
	}
	if _, found := rowOf(t, st, codeB, KeyDecision); found {
		t.Errorf("证据不全的乙不应有决策行")
	}
	if c := mustRow(t, st, codeB, KeyNews); c.Status != model.TraceFail {
		t.Errorf("漏答应留下 failed 行（当天重跑会重试），实际 %q", c.Status)
	}
}

// TestReviewEvidenceFailureYieldsNoDecision 整条 prompt 问不出来时不许凑决策：
// 全部候选记 Failed（不是"评审说不买"），且不写任何决策行。
func TestReviewEvidenceFailureYieldsNoDecision(t *testing.T) {
	st := testStore(t)
	srv := newStub(t, allOKBatch(), map[string]int{markValue: 9})
	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatalf("模型故障不应中断流程: %v", err)
	}
	for _, code := range []string{codeA, codeB} {
		if d := got[code]; !d.Failed || d.Approve {
			t.Errorf("%s 应记为评审失败而非否决: %+v", code, d)
		}
		if _, found := rowOf(t, st, code, KeyDecision); found {
			t.Errorf("证据不全时 %s 不应产生决策行", code)
		}
		if mustRow(t, st, code, KeyValue).Status != model.TraceFail {
			t.Errorf("%s 的失败维度应落 failed", code)
		}
		if mustRow(t, st, code, KeyTech).Verdict == "" {
			t.Errorf("%s 成功维度的结论应保留（重跑不必重问）", code)
		}
	}
}

// TestReviewFailedPromptIsRetried failed 行不算缓存命中：一次网络抖动不能把当天锁死。
func TestReviewFailedPromptIsRetried(t *testing.T) {
	st := testStore(t)
	req := batchReq(item(codeA, "甲")) // 单只即可，重点是重问次数
	bad := newStub(t, allOKBatch(), map[string]int{markTech: 9})
	if _, err := newReviewer(bad.srv.URL, st, true).DecideBatch(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if mustRow(t, st, codeA, KeyTech).Status != model.TraceFail {
		t.Fatalf("首轮应留下 failed 行")
	}
	if _, found := rowOf(t, st, codeA, KeyDecision); found {
		t.Errorf("技术形态没问出来时不该有决策行")
	}

	good := newStub(t, allOKBatch(), nil)
	got, err := newReviewer(good.srv.URL, st, true).DecideBatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !got[codeA].Approve {
		t.Errorf("故障恢复后应能问出结论: %+v", got[codeA])
	}
	if mustRow(t, st, codeA, KeyTech).Status != model.TraceOK {
		t.Errorf("failed 行应被成功结果覆盖")
	}
	// 首轮已答好其余三条证据（决策因证据不全没问），所以第二轮只需补问 tech 与 decision。
	if good.total() != 2 {
		t.Errorf("重跑只应补问 tech 与 decision 两次，实际 %d", good.total())
	}
}

// TestReviewFencedJSONParses 模型爱包 ```json 围栏：解析必须容得下，否则当天白跑。
func TestReviewFencedJSONParses(t *testing.T) {
	st := testStore(t)
	replies := allOKBatch()
	replies[markDecide] = []string{"```json\n" + replies[markDecide][0] + "\n```"}
	srv := newStub(t, replies, nil)
	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatal(err)
	}
	if !got[codeA].Approve {
		t.Errorf("围栏内的 JSON 应能解析: %+v", got[codeA])
	}
}

// TestReviewBadAnswerRetriedThenRecorded 答案不合契约（整批 JSON 半截）时重问同一问；
// 三次都坏才逐标的记失败行，并保留模型原文供人判断。
func TestReviewBadAnswerRetriedThenRecorded(t *testing.T) {
	st := testStore(t)
	replies := allOKBatch()
	truncated := `{"results":[{"ts_code":"600001.SH","approve":true,"weight_pct":0.3,"confidence":0.6,"reason":"趋势不错，量比配合`
	replies[markDecide] = []string{truncated} // 用尽后重复最后一条 → 三次都截断
	srv := newStub(t, replies, nil)

	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatalf("DecideBatch 链路本身不该报错: %v", err)
	}
	for _, code := range []string{codeA, codeB} {
		if !got[code].Failed {
			t.Errorf("三次都没问出契约要求的答案，%s 必须记失败而不是当成都不买: %+v", code, got[code])
		}
	}
	if n := srv.callsOf(markDecide); n != 3 {
		t.Errorf("决策应最多问 3 次，实际 %d 次", n)
	}
	row := mustRow(t, st, codeA, KeyDecision)
	if row.Status != model.TraceFail || !strings.Contains(row.Error, "ts_code") {
		t.Errorf("失败行要带上模型原文片段，实际 %+v", row)
	}
}

// TestReviewEmptyAnswerIsRetried 模型只吐 reasoning 就 completed 收尾（实测真发生过：
// out=368 且 reasoning=368、正文 0 字）时，同一问重问到有答案为止。
func TestReviewEmptyAnswerIsRetried(t *testing.T) {
	st := testStore(t)
	replies := allOKBatch()
	replies[markTech] = []string{stubNoMessage, stubNoMessage, replies[markTech][0]}
	srv := newStub(t, replies, nil)

	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatalf("空答案重问后应成功: %v", err)
	}
	if got[codeA].Failed {
		t.Errorf("第三次答好了就不该记失败: %+v", got[codeA])
	}
	if n := srv.callsOf(markTech); n != 3 {
		t.Errorf("两次空答案 + 一次成功 = 3 次，实际 %d", n)
	}
}

// TestReviewTransportFailureIsRetriedOnce 只有"这次请求没成"（服务端 5xx）允许原样重问一次。
func TestReviewTransportFailureIsRetriedOnce(t *testing.T) {
	st := testStore(t)
	srv := newStub(t, allOKBatch(), map[string]int{markTech: 1}) // 第一次 500，第二次正常
	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatalf("传输层失败重问一次后应成功: %v", err)
	}
	if got[codeA].Failed {
		t.Errorf("重问成功后不该把 codeA 记成失败: %+v", got[codeA])
	}
	if n := srv.callsOf(markTech); n != 2 {
		t.Errorf("tech 应问 2 次（首次 500 + 重问），实际 %d 次", n)
	}
}

// TestReviewShortCodeMatches 模型常把 "600001.SH" 写成 "600001"：按 6 位代码认，不能算漏答。
func TestReviewShortCodeMatches(t *testing.T) {
	st := testStore(t)
	replies := allOKBatch()
	replies[markTech] = []string{evResults("600001", "positive", "形态好", "600002", "neutral", "横盘")}
	srv := newStub(t, replies, nil)
	got, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{codeA, codeB} {
		if got[code].Failed {
			t.Errorf("%s 用短代码回传也应认出来，实得 %+v", code, got[code])
		}
	}
}

// TestReviewDisabledMeansNoBuy llm.enabled=false 时一律不批：
// 没有决策者就等于不开仓，绝不偷偷退回规则信号那条老路。
func TestReviewDisabledMeansNoBuy(t *testing.T) {
	st := testStore(t)
	srv := newStub(t, allOKBatch(), nil)
	r := newReviewer(srv.srv.URL, st, false)

	if r.Enabled() {
		t.Fatalf("未启用时 Enabled 必须为 false")
	}
	got, err := r.DecideBatch(context.Background(), batchReq())
	if err != nil {
		t.Fatal(err)
	}
	for code, d := range got {
		if d.Approve || d.Failed {
			t.Errorf("%s 未启用必须不批且不记失败: %+v", code, d)
		}
	}
	if srv.total() != 0 {
		t.Errorf("未启用不应发起任何 HTTP 调用，实际 %d", srv.total())
	}
}

// TestReviewOnlyNewsSearches 联网边界：只有消息面那条挂 web_search，
// 且用的就是配置的 high 档；其余三条证据与决策都不挂。
func TestReviewOnlyNewsSearches(t *testing.T) {
	st := testStore(t)
	srv := newStub(t, allOKBatch(), nil)
	if _, err := newReviewer(srv.srv.URL, st, true).DecideBatch(context.Background(), batchReq()); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{markTech, markValue, markSector, markDecide} {
		if sizes := srv.sizesOf(m); len(sizes) != 1 || sizes[0] != "" {
			t.Errorf("%s 不应挂检索，实际 %v", m, sizes)
		}
	}
	if sizes := srv.sizesOf(markNews); len(sizes) != 1 || sizes[0] != "high" {
		t.Errorf("消息面检索档=%v，期望 [high]（配置值）", sizes)
	}
}

// TestExtractJSON 掐 JSON 的边界：无花括号时原样返回（交给上层报"不可解析"）。
func TestExtractJSON(t *testing.T) {
	if got := extractJSON("前缀 {\"a\":1} 后缀"); got != `{"a":1}` {
		t.Errorf("提取结果=%q", got)
	}
	if got := extractJSON("完全不是 JSON"); got != "完全不是 JSON" {
		t.Errorf("无花括号时应原样返回，实际 %q", got)
	}
}
