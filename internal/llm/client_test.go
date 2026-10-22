package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCompleteToolCallLeakIsNoAnswer 服务端没执行工具、模型把调用写成正文标签时，
// 必须归到"这一问没拿到答案"（可重问），不能当成"给了正文但不合契约"。
//
// 2026-10-09 实测形态：挂 web_search 的 20 只批次，status=completed、output 里只有
// reasoning + message，而 message 正文是 <｜｜DSML｜｜ calls>…</｜｜DSML｜｜ calls>。
// 判成解析失败会让上层把它误诊成模型不听话；判成空答案才会真的再问一次。
func TestCompleteToolCallLeakIsNoAnswer(t *testing.T) {
	leaked := `<｜｜DSML｜｜ calls>
<｜｜DSML｜｜ invoke name="web_search">
<｜｜DSML｜｜ parameter name="query" string="true">淮河能源 600575 立案调查 风险公告</｜｜DSML｜｜ parameter>
</｜｜DSML｜｜ invoke>
</｜｜DSML｜｜ calls>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"text","text":%q}]}]}`, leaked)
	}))
	defer srv.Close()

	c := NewClient("k", srv.URL, "m", "high", nil)
	_, err := c.Complete(context.Background(), Request{System: "s", User: "u", Search: true})
	if !errors.Is(err, errNoAnswer) {
		t.Fatalf("工具调用泄漏应判为可重问的空答案，实际 err=%v", err)
	}
	if !strings.Contains(err.Error(), "tools 未被服务端执行") {
		t.Errorf("错误必须点名真因（工具没被执行），实际: %v", err)
	}
}

// TestCompleteNormalJSONNotFlaggedAsLeak 正常 JSON 答案不得被泄漏检测误伤。
func TestCompleteNormalJSONNotFlaggedAsLeak(t *testing.T) {
	ans := `{"results":[{"ts_code":"600575.SH","stance":"unknown","finding":"无公告数据","unknown":true}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"completed","output":[{"type":"message","content":[{"type":"text","text":%q}]}]}`, ans)
	}))
	defer srv.Close()

	c := NewClient("k", srv.URL, "m", "high", nil)
	got, err := c.Complete(context.Background(), Request{System: "s", User: "u"})
	if err != nil {
		t.Fatalf("正常答案不应报错: %v", err)
	}
	if got != ans {
		t.Errorf("答案被改写: %q", got)
	}
}

// TestDecisionPromptReadsUnknownAsNoVeto 组长 seq=141 追加的两条决策口径，同夜随 P0/P1/P2 上线：
// ① news=unknown 不许被读成反对意见（只说"不加分不减分"不够，保守读法仍会把整维恒等于否决）；
// ② ETF 的否决依据里不能再要求"当期 ETF 层面公告"—— 摘掉检索后这个前提永久够不着，
//
//	留着等于把 ETF 真实有的技术形态与流动性证据一起废掉。
func TestDecisionPromptReadsUnknownAsNoVeto(t *testing.T) {
	for _, want := range []string{"不构成不买的理由", "缺的是正证据，不是减分", "只凭其余维度"} {
		if !strings.Contains(decisionSystem, want) {
			t.Errorf("决策 prompt 缺 %q：消息面 unknown 会被当成否决票，整个维度恒等于不买", want)
		}
	}
	if strings.Contains(decisionSystem, "当期 ETF 层面公告") {
		t.Error("ETF 条款仍以「当期 ETF 层面公告」为否决依据：本系统没有公告源，这个前提够不着")
	}
	if !strings.Contains(decisionSystem, "跟踪方向、流动性、价格形态与价格异常证据") {
		t.Error("ETF 条款必须列出它实际拿得到的四类证据")
	}
	// 反向护栏：negative 仍是硬信号，对称规则不能被写成"unknown 与 negative 一样处理"。
	if !strings.Contains(decisionSystem, "它答 negative 表示价格形态已经异常") {
		t.Error("negative 优先处理的口径不得被对称规则稀释")
	}
	// 成本口径：审批前必须换算成本，覆盖不了双边成本的单子不许批。
	if !strings.Contains(decisionSystem, "交易成本是硬成本") ||
		!strings.Contains(decisionSystem, "覆盖不了成本的也不要批") {
		t.Error("决策 prompt 缺少交易成本/最低净收益约束")
	}
}

// TestNewsPromptPromisesNoSearch news 维度的两条硬约束：不挂检索工具、不许声称有工具。
//
// 这是 P1（agent_issues：模型返回 XML 工具调用标签而非 JSON）的回归锁。
// 只要有人把 Search 改回 true 或在 prompt 里写回"你会拿到 web_search 工具"，
// 20 只批次就会重新整批解析失败，当日零计划。
func TestNewsPromptPromisesNoSearch(t *testing.T) {
	var news *PromptSpec
	for i := range EvidencePrompts {
		if EvidencePrompts[i].Key == KeyNews {
			news = &EvidencePrompts[i]
		}
	}
	if news == nil {
		t.Fatal("EvidencePrompts 里没有 news 维度")
	}
	if news.Search {
		t.Error("news 维度不得再挂 web_search 工具：本端点从不执行它（见 prompts.go 联网边界）")
	}
	for _, p := range EvidencePrompts {
		if p.Search {
			t.Errorf("%s 维度挂了检索工具，违反联网边界", p.Key)
		}
		for _, bad := range []string{"你会拿到 web_search 工具", "本维度允许检索", "请用它逐只查"} {
			if strings.Contains(p.System, bad) {
				t.Errorf("%s 的 prompt 仍声称有检索能力（%q）", p.Key, bad)
			}
		}
	}
	for _, want := range []string{"不联网", "无公告", "unknown"} {
		if !strings.Contains(news.System, want) {
			t.Errorf("news prompt 缺 %q：必须明说没有数据源并给出 unknown 口径，实际:\n%s", want, news.System)
		}
	}
}
