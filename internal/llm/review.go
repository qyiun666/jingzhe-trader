package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
)

// 留痕的结果词汇直接用 model.TraceOK / model.TraceFail（这一行就是 run_trace 的一行）。
// 失败行永远留痕，且失败行不算缓存命中 —— 下次重跑会重试，
// 否则一次网络抖动就把这只票当天锁死。
const (
	stanceUnknown = "unknown"
	verdictBuy    = "buy"
	verdictSkip   = "skip"
)

// Reviewer 买入决策者：四条证据 prompt + 一条决策 prompt，**每条整批问一次**。
//
// 四条硬约定：
//   - 一次调用覆盖当日全部候选（用户拍板：筛选后一次性发过去）。逐只问会把 5 条 prompt
//     变成 5N 次调用，联网那条还要把上下文重烧 N 遍；
//   - 每条 prompt 每标每日只问一次：先查当日这条 llm 轨迹，命中即复用，重跑只问没答过的那几只；
//   - 证据没问出来的标的 → 不批（Failed=true），绝不拿残缺证据凑一个决策；
//   - 未启用（llm.enabled=false）→ 一律不批。没有决策者时偷偷退回规则信号，
//     等于把刚拆掉的那道"综合分/均线门槛"装回来 —— 那正是长期不出计划的原因。
//
// 缓存键是 日+标的+prompt_key，不含提示词文本：改提示词不会让当日重跑重新提问
// （刻意如此，同日重跑必须得到同一个答案），提示词改动次日生效。
type Reviewer struct {
	client  *Client
	st      *store.Store
	enabled bool
	now     func() time.Time
	backoff time.Duration // 重试退避基数（生产 2s；测试注入毫秒级，不等真秒）
}

// NewReviewer 构造决策者。enabled 由调用方从配置传入；key/model 缺失时自动视为关闭。
func NewReviewer(client *Client, st *store.Store, enabled bool) *Reviewer {
	return &Reviewer{
		client:  client,
		st:      st,
		enabled: enabled && client.Enabled(),
		now:     time.Now,
		backoff: defaultRetryBackoff,
	}
}

// defaultRetryBackoff 第 N 次重问前等 N×2 秒：模型侧的失败大多是瞬时的限流/网络抖动，
// 立刻重问等于再撞一次。
const defaultRetryBackoff = 2 * time.Second

// WithClock 注入时钟（测试用）。
func (r *Reviewer) WithClock(f func() time.Time) *Reviewer {
	if f != nil {
		r.now = f
	}
	return r
}

// WithRetryBackoff 注入退避基数（测试用；传 0 表示重问前不等待）。
func (r *Reviewer) WithRetryBackoff(d time.Duration) *Reviewer {
	r.backoff = d
	return r
}

// Enabled 买入决策是否真的在跑（调度器据此记降级并汇总告警）。
func (r *Reviewer) Enabled() bool { return r != nil && r.enabled }

// evidenceRow 一条证据 prompt 对一只票的回答。
type evidenceRow struct {
	TsCode  string `json:"ts_code"`
	Stance  string `json:"stance"`
	Finding string `json:"finding"`
	Unknown bool   `json:"unknown"`
}

// decisionRow 决策 prompt 对一只票的回答。
type decisionRow struct {
	TsCode     string  `json:"ts_code"`
	Approve    bool    `json:"approve"`
	WeightPct  float64 `json:"weight_pct"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

type evidenceReply struct {
	Results []evidenceRow `json:"results"`
}

type decisionReply struct {
	Results []decisionRow `json:"results"`
}

// DecideBatch 实现 signal.BuyDecider：整批候选 → 四条证据各问一次 → 一次总裁决。
//
// 返回的 error 只表示写库失败这类链路故障；模型侧任何"问不出来"都翻译成
// Approve=false + Failed=true 摆在 map 里，由调度器汇总成一条告警，而不是每只票刷一条。
func (r *Reviewer) DecideBatch(ctx context.Context, req signal.BatchRequest) (map[string]signal.BuyDecision, error) {
	out := make(map[string]signal.BuyDecision, len(req.Items))
	if !r.Enabled() {
		for _, it := range req.Items {
			out[it.Candidate.TsCode] = signal.BuyDecision{Reason: "llm.enabled=false：没有决策者，当日不开新仓"}
		}
		return out, nil
	}
	conclusions, broken, err := r.reviewEvidence(ctx, req)
	if err != nil {
		return nil, err
	}
	ready := make([]signal.BuyRequest, 0, len(req.Items))
	for _, it := range req.Items {
		code := it.Candidate.TsCode
		if why, bad := broken[code]; bad {
			out[code] = signal.BuyDecision{Failed: true, Reason: clip(why, 200)}
			continue
		}
		ready = append(ready, it)
	}
	if err := r.decideBatch(ctx, batchState{date: req.TradeDate, items: ready,
		conclusions: conclusions, out: out}); err != nil {
		return nil, err
	}
	return out, nil
}

// reviewEvidence 四条证据各问一次（整批），返回每只票的结论行与问坏的票（→ 原因）。
func (r *Reviewer) reviewEvidence(ctx context.Context, req signal.BatchRequest) (map[string][]string, map[string]string, error) {
	lines := make(map[string][]string, len(req.Items))
	broken := make(map[string]string)
	for _, p := range EvidencePrompts {
		got, err := r.askEvidence(ctx, askAsk{date: req.TradeDate, spec: p, items: req.Items})
		if err != nil {
			return nil, nil, err
		}
		for _, it := range req.Items {
			code := it.Candidate.TsCode
			row, ok := got[code]
			if !ok {
				if _, bad := broken[code]; !bad {
					broken[code] = fmt.Sprintf("%s评审未问出结果", p.Title)
				}
				continue
			}
			lines[code] = append(lines[code], conclusionLine(p, row))
		}
	}
	return lines, broken, nil
}

// askEvidence 问一条证据 prompt（整批未答过的标的），返回按 ts_code 索引的结论。
//
// 模型没答、答崩了、答漏了都只反映在"返回的 map 里少这几个标的"，不当 error：
// 上层据此把这些票标成 Failed，其余答好的照常进决策。只有写库失败才是链路 error。
func (r *Reviewer) askEvidence(ctx context.Context, a askAsk) (map[string]evidenceRow, error) {
	out := make(map[string]evidenceRow, len(a.items))
	var pending []signal.BuyRequest
	for _, it := range a.items {
		code := it.Candidate.TsCode
		cached, found, err := r.st.LLMRepo().GetCall(ctx, a.date, code, a.spec.Key)
		if err != nil {
			return nil, err
		}
		if found && cached.Status == model.TraceOK {
			out[code] = evidenceRow{TsCode: code, Stance: cached.Verdict, Finding: cached.Rationale}
			continue
		}
		pending = append(pending, it)
	}
	if len(pending) == 0 {
		return out, nil
	}
	a.items = pending
	a.user = evidenceUser(a.spec, pending)
	var reply evidenceReply
	if err := r.ask(ctx, a, fillEvidence(&reply)); err != nil {
		return out, nil
	}
	matched := indexResults(reply.Results, func(e evidenceRow) string { return e.TsCode })
	for _, it := range pending {
		code := it.Candidate.TsCode
		row, ok := matched[canonCode(code)]
		if !ok {
			if err := r.markFailed(ctx, a, code, "批量响应里没有这个标的"); err != nil {
				return nil, err
			}
			continue
		}
		if row.Stance == "" || row.Unknown {
			row.Stance = stanceUnknown
		}
		if err := r.save(ctx, model.LLMCall{TradeDate: a.date, TsCode: code, PromptKey: a.spec.Key,
			Verdict: row.Stance, Rationale: row.Finding}, ""); err != nil {
			return nil, err
		}
		out[code] = row
	}
	return out, nil
}

// batchState 一次批量裁决的全部上下文（收成一个结构，避免五个位置参数）。
type batchState struct {
	date        string
	items       []signal.BuyRequest
	conclusions map[string][]string
	out         map[string]signal.BuyDecision
}

// decideBatch 问一次总裁决（整批）；已缓存的标的不重问，问坏的标的记 Failed。
func (r *Reviewer) decideBatch(ctx context.Context, b batchState) error {
	var pending []signal.BuyRequest
	for _, it := range b.items {
		code := it.Candidate.TsCode
		cached, found, err := r.st.LLMRepo().GetCall(ctx, b.date, code, KeyDecision)
		if err != nil {
			return err
		}
		if !found || cached.Status != model.TraceOK {
			pending = append(pending, it)
			continue
		}
		// weight_pct 必须一起还原：否则重跑会把首轮批了的买单判成"未给仓位 = 不买"。
		b.out[code] = decisionToSignal(decisionRow{TsCode: code, Approve: cached.Verdict == verdictBuy,
			WeightPct: cached.WeightPct, Confidence: cached.Confidence, Reason: cached.Rationale})
	}
	if len(pending) == 0 {
		return nil
	}
	a := askAsk{date: b.date, spec: PromptSpec{Key: KeyDecision, Title: "买入决策", System: decisionSystem},
		items: pending, user: decisionUser(pending, b.conclusions)}
	var reply decisionReply
	if err := r.ask(ctx, a, fillDecision(&reply)); err != nil {
		for _, it := range pending {
			b.out[it.Candidate.TsCode] = signal.BuyDecision{Failed: true,
				Reason: clip("决策评审失败："+err.Error(), 200)}
		}
		return nil
	}
	matched := indexResults(reply.Results, func(d decisionRow) string { return d.TsCode })
	for _, it := range pending {
		code := it.Candidate.TsCode
		row, ok := matched[canonCode(code)]
		if !ok {
			if err := r.markFailed(ctx, a, code, "批量响应里没有这个标的"); err != nil {
				return err
			}
			b.out[code] = signal.BuyDecision{Failed: true, Reason: "批量决策响应里没有这个标的"}
			continue
		}
		row.Approve = row.Approve && row.WeightPct > 0 // 批了但没给钱 = 没批
		if err := r.save(ctx, model.LLMCall{TradeDate: b.date, TsCode: code, PromptKey: KeyDecision,
			Verdict: verdictOf(row), Confidence: row.Confidence, WeightPct: row.WeightPct,
			Rationale: row.Reason}, ""); err != nil {
			return err
		}
		b.out[code] = decisionToSignal(row)
	}
	return nil
}

// askAsk 一次批量 prompt 调用的全部输入（参数收成一个结构，避免位置参数过长）。
type askAsk struct {
	date  string
	spec  PromptSpec
	items []signal.BuyRequest
	user  string
}

// ask 问一批：调模型 → 解析 {"results":[...]}，最多问 askAttempts 次，任何一次成功即返回。
//
// decode 由调用方给（每次尝试都从空白的接收结构开始，避免上一次的半成品混进来）。
// 全部尝试都失败时，给这批每个标的各记一条失败行，并返回汇总错误。
//
// 为什么允许重问同一问（实测 2026-09-04，同一批 6 只票）：失败分三类，都不代表
// "模型判断过并且给了答案" —— ① 传输层没拿到响应；② status=completed 但 output 里
// 没有 message 条目（out=368 全是 reasoning，模型空手收尾）；③ 给了正文但 JSON 半截。
// 重问不会把"没结论"粉饰成"有结论"：三次都没结论就逐标的记失败行，
// 调度器据此落 LLM_FAILED urgent 告警，当日零计划是看得见、可 trigger_task 补跑的。
//
// 与之相对，以前那套"按检索档轮流问"（high 问不出降 low）已经删掉：它是对②③的误诊，
// 真因之一是请求没传 max_output_tokens（见 client 的 maxOutputTokens），
// 另一个成因（空手收尾）与档位无关。
const askAttempts = 3

func (r *Reviewer) ask(ctx context.Context, a askAsk, decode func(string) error) error {
	var lastErr error
	for attempt := 1; attempt <= askAttempts; attempt++ {
		if attempt > 1 {
			observability.S().Warnw("prompt 未问出结果，重问同一问",
				"prompt", a.spec.Key, "date", a.date, "attempt", attempt, "of", askAttempts,
				"codes", len(a.items), "err", clip(lastErr.Error(), 200))
			if perr := r.pauseBeforeRetry(ctx, attempt); perr != nil {
				return r.quoteFailure(ctx, a, perr)
			}
		}
		raw, cerr := r.client.Complete(ctx, Request{
			System: a.spec.System, User: a.user, Search: a.spec.Search})
		if cerr != nil {
			lastErr = cerr
			continue
		}
		if lastErr = decode(raw); lastErr == nil {
			return nil
		}
	}
	return r.quoteFailure(ctx, a, lastErr)
}

// pauseBeforeRetry 重试前退避（backoff × 第几次），ctx 取消时把错误交回调用方记失败行。
func (r *Reviewer) pauseBeforeRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt) * r.backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("重试第 %d 次前上下文已取消: %w", attempt, ctx.Err())
	case <-timer.C:
		return nil
	}
}

// quoteFailure 整批问崩了：给每个待问标的各记一条失败行（不静默丢弃）。
func (r *Reviewer) quoteFailure(ctx context.Context, a askAsk, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("未知失败")
	}
	for _, it := range a.items {
		if err := r.markFailed(ctx, a, it.Candidate.TsCode, cause.Error()); err != nil {
			return err
		}
	}
	return fmt.Errorf("prompt %s 整批失败: %w", a.spec.Key, cause)
}

// markFailed 给一只票的一条 prompt 记失败行（当天重跑会重试它）。
func (r *Reviewer) markFailed(ctx context.Context, a askAsk, code, why string) error {
	return r.save(ctx, model.LLMCall{TradeDate: a.date, TsCode: code, PromptKey: a.spec.Key,
		Rationale: "未质证"}, clip(why, 300))
}

// save 落一条 LLM 轨迹（run_trace 的 llm:<标的>:<prompt_key> 行）；
// rationale 对证据行存 finding，对决策行存 reason。
//
// failMsg 非空即按失败记账：失败行不写 verdict（免得被当成一次有效结论复用），
// 当天重跑会重试它。这里**只返回写库错误** —— "模型没答出来"是业务结果，
// 不是链路故障，借 error 传会让调用方误以为整批中断。
func (r *Reviewer) save(ctx context.Context, call model.LLMCall, failMsg string) error {
	call.Status = model.TraceOK
	call.CreatedAt = r.now().UTC().Format(time.RFC3339)
	if failMsg != "" {
		call.Status = model.TraceFail
		call.Verdict = ""
		call.Error = clip(failMsg, 300)
	}
	return r.st.LLMRepo().SaveCall(ctx, call)
}

// fillEvidence 返回一个"解析进这个 reply"的 decode 闭包：每次尝试先把目标清空，
// 免得上一问解析到一半的字段混进这一问的答案里。
func fillEvidence(dst *evidenceReply) func(string) error {
	return func(raw string) error {
		*dst = evidenceReply{}
		return decodeResults(raw, dst)
	}
}

// fillDecision 同 fillEvidence，决策行的接收结构不同。
func fillDecision(dst *decisionReply) func(string) error {
	return func(raw string) error {
		*dst = decisionReply{}
		return decodeResults(raw, dst)
	}
}

// verdictOf 决策行落库的 verdict：批准且给了仓位才算 buy。
func verdictOf(d decisionRow) string {
	if d.Approve && d.WeightPct > 0 {
		return verdictBuy
	}
	return verdictSkip
}

// decisionToSignal 把模型的一条决策结论翻成领域裁决（含"批了没给钱"这类自相矛盾的输出）。
func decisionToSignal(d decisionRow) signal.BuyDecision {
	switch {
	case !d.Approve:
		return signal.BuyDecision{Reason: clip("不买："+d.Reason, 240)}
	case !(d.WeightPct > 0): // 同时挡住 NaN：没给出可信比例就等于没批
		return signal.BuyDecision{Reason: clip("批准但未给出有效仓位比例，按不买处理："+d.Reason, 240)}
	default:
		return signal.BuyDecision{
			Approve: true, WeightPct: d.WeightPct, Confidence: d.Confidence,
			Reason: clip(fmt.Sprintf("买入 %.0f%% 仓位（置信 %.2f）：%s",
				d.WeightPct*100, d.Confidence, d.Reason), 240),
		}
	}
}

// conclusionLine 把一条证据结论拼进决策 prompt 正文。
func conclusionLine(p PromptSpec, e evidenceRow) string {
	return fmt.Sprintf("【%s】%s：%s", p.Title, e.Stance, clip(e.Finding, 80))
}

// indexResults 把模型返回的 results 按标的索引；键归一化到 6 位代码，
// 因为模型常把 "600001.SH" 写成 "600001"。
func indexResults[T any](rows []T, codeOf func(T) string) map[string]T {
	out := make(map[string]T, len(rows))
	for _, row := range rows {
		out[canonCode(codeOf(row))] = row
	}
	return out
}

// canonCode 归一化标的代码：去后缀、转大写。
func canonCode(ts string) string {
	s := strings.TrimSpace(ts)
	if i := strings.IndexByte(s, '.'); i > 0 {
		s = s[:i]
	}
	return strings.ToUpper(s)
}

// shortCode 去掉交易所后缀，检索用"名称 + 6 位代码"比带后缀的 ts_code 命中率高。
func shortCode(ts string) string { return canonCode(ts) }

// decodeResults 把模型回复解进 out（先掐掉 ```json 围栏与前后说明文字）。
func decodeResults(raw string, out any) error {
	if err := json.Unmarshal([]byte(extractJSON(raw)), out); err != nil {
		return fmt.Errorf("输出不可解析为 JSON: %w（原文 %s）", err, clip(raw, 160))
	}
	return nil
}

// extractJSON 从模型回复里掐出第一个 { 到最后一个 }：即便它加了 ```json 围栏也能解析。
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end < start {
		return s
	}
	return s[start : end+1]
}

// clip 按字符数截断（模型产出的 reason/finding 要进邮件与日志，长度必须收口；
// 按字节截断会把中文切成半个 UTF-8 字符）。
func clip(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n]) + "…"
}
