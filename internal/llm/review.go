package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	// deadline 整轮评审（四条证据 + 决策）的总时限；promptTimeout 是单条 prompt
	// （含其内部重试）的时限。二者都必须小于调度器给 evening_pipeline 的 30 分钟，
	// 否则 4 条并行证据里只要有一条挂死，就会拖到任务级超时，连失败行都来不及落。
	deadline      time.Duration
	promptTimeout time.Duration
}

// 评审时限：全局 12 分钟、单 prompt 10 分钟。
//
// 为什么是这两个数：单次 HTTP 上限 600s（见 NewClient），四条证据并行后整轮耗时
// 约等于最慢的一条；留 2 分钟给决策 prompt 与落库。全局 12 分钟远小于任务级
// 30 分钟，保证"评审自己失败"先于"任务被外部掐死"发生。
const (
	defaultReviewDeadline = 12 * time.Minute
	defaultPromptTimeout  = 10 * time.Minute
	traceWriteTimeout     = 5 * time.Second
)

// NewReviewer 构造决策者。enabled 由调用方从配置传入；key/model 缺失时自动视为关闭。
func NewReviewer(client *Client, st *store.Store, enabled bool) *Reviewer {
	return &Reviewer{
		client:        client,
		st:            st,
		enabled:       enabled && client.Enabled(),
		now:           time.Now,
		backoff:       defaultRetryBackoff,
		deadline:      defaultReviewDeadline,
		promptTimeout: defaultPromptTimeout,
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

// WithTimeouts 注入整轮与单 prompt 时限（测试用；非正值保持默认）。
// 单 prompt 时限不得超过整轮时限：否则内层永远等不到自己的超时，外层先取消。
func (r *Reviewer) WithTimeouts(deadline, prompt time.Duration) *Reviewer {
	if deadline > 0 {
		r.deadline = deadline
	}
	if prompt > 0 && prompt <= r.deadline {
		r.promptTimeout = prompt
	}
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
	// 整轮 deadline：四条证据并行 + 一次决策，任何一条挂死都必须在 12 分钟内收口。
	// 调用方（evening_pipeline）另有 30 分钟任务级超时，这里先到期才能把失败行落库、
	// 把 LLM_FAILED 降级写进当日轨迹，而不是整条任务被外部掐死。
	if r.deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.deadline)
		defer cancel()
	}
	conclusions, broken, err := r.reviewEvidence(ctx, req)
	if err != nil {
		return nil, err
	}
	// 整轮 deadline 已到：即便四条证据恰好都答完了，也没有时间再问决策。
	// 把全部候选按"评审失败"收口，而不是拿一个已过期的 ctx 去调下一次模型。
	if ctx.Err() != nil {
		for _, it := range req.Items {
			if _, bad := broken[it.Candidate.TsCode]; !bad {
				broken[it.Candidate.TsCode] = "评审超时，未进入决策"
			}
		}
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

// reviewEvidence 四条证据并行各问一次（整批），返回每只票的结论行与问坏的票（→ 原因）。
//
// 并行是这轮改动的核心：四条证据彼此独立、都只读同一批候选，串行时整轮耗时是四问之和
// （单问实测可达数分钟），并行后约等于最慢的一条。这里没有引入 errgroup 依赖，
// 用 WaitGroup 达到同样语义，但**不因第一条失败取消其余三条**：每条 prompt 的失败行
// 都要落库，已答好的证据也要留给当日重跑复用；DB 错误在全部结束后按固定顺序返回。
func (r *Reviewer) reviewEvidence(ctx context.Context, req signal.BatchRequest) (map[string][]string, map[string]string, error) {
	type outcome struct {
		spec PromptSpec
		got  map[string]evidenceRow
	}
	outs := make([]outcome, len(EvidencePrompts))
	errs := make([]error, len(EvidencePrompts))
	var wg sync.WaitGroup
	for i := range EvidencePrompts {
		p := EvidencePrompts[i]
		wg.Add(1)
		go func(i int, p PromptSpec) {
			defer wg.Done()
			pctx := ctx
			if r.promptTimeout > 0 {
				var cancel context.CancelFunc
				pctx, cancel = context.WithTimeout(ctx, r.promptTimeout)
				defer cancel()
			}
			started := time.Now()
			got, err := r.askEvidence(pctx, askAsk{date: req.TradeDate, spec: p, items: req.Items})
			outs[i] = outcome{spec: p, got: got}
			errs[i] = err
			fields := []any{"prompt", p.Key, "date", req.TradeDate, "codes", len(req.Items),
				"answered", len(got), "secs", time.Since(started).Seconds()}
			if err != nil {
				observability.S().Errorw("证据 prompt 链路失败", append(fields, "err", err.Error())...)
			} else {
				observability.S().Infow("证据 prompt 完成", fields...)
			}
		}(i, p)
	}
	wg.Wait()
	// 单 prompt 超时与整轮 deadline 都按"评审没问出来"处理，不当作链路故障：
	// 否则一次超时会被翻译成整条 evening_pipeline 失败，反而没有"证据不全不批"的收口。
	// 只有非上下文类的 DB 错误才上抛。
	ctxFailed := ctx.Err() != nil
	for _, err := range errs {
		if err != nil && isCtxErr(err) {
			ctxFailed = true
		}
	}
	if ctxFailed {
		for i, err := range errs {
			if err == nil {
				continue
			}
			for _, it := range req.Items {
				code := it.Candidate.TsCode
				if _, answered := outs[i].got[code]; answered {
					continue
				}
				_ = r.markFailed(ctx, askAsk{date: req.TradeDate, spec: outs[i].spec},
					code, "评审超时，未获得证据")
			}
		}
	} else {
		for _, err := range errs {
			if err != nil {
				return nil, nil, err
			}
		}
	}

	lines := make(map[string][]string, len(req.Items))
	broken := make(map[string]string)
	for _, o := range outs {
		for _, it := range req.Items {
			code := it.Candidate.TsCode
			row, ok := o.got[code]
			if !ok {
				if _, bad := broken[code]; !bad {
					broken[code] = fmt.Sprintf("%s评审未问出结果", o.spec.Title)
				}
				continue
			}
			lines[code] = append(lines[code], conclusionLine(o.spec, row))
		}
	}
	return lines, broken, nil
}

// askEvidence 问一条证据 prompt（整批未答过的标的），返回按 ts_code 索引的结论。
//
// 模型没答、答崩了、答漏了都只反映在"返回的 map 里少这几个标的"，不当 error：
// 上层据此把这些票标成 Failed，其余答好的照常进决策。只有写库失败才是链路 error。
func (r *Reviewer) askEvidence(ctx context.Context, a askAsk) (map[string]evidenceRow, error) {
	if ctx.Err() != nil {
		// 全局/单 prompt deadline 已到：不再读缓存（用已取消的 ctx 读库会返回链路错误，
		// 把一次超时误诊成数据库故障），直接给本批每个标的记失败行。
		for _, it := range a.items {
			if err := r.markFailed(ctx, a, it.Candidate.TsCode, "评审超时，未获得证据"); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
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
// 另一个成因（空手收尾）与检索档无关。
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
		} else if lastErr = decode(raw); lastErr == nil {
			return nil
		}
		if !retryable(lastErr) {
			observability.S().Warnw("prompt 失败不可重试，停止重问",
				"prompt", a.spec.Key, "date", a.date, "attempt", attempt,
				"codes", len(a.items), "err", clip(lastErr.Error(), 200))
			return r.quoteFailure(ctx, a, lastErr)
		}
	}
	return r.quoteFailure(ctx, a, lastErr)
}

// retryable 判断这次失败再问一遍是否有意义。
//
// 可重问：传输层抖动（连不上/5xx/响应体不是 JSON）、空答案与工具调用泄漏
// （errNoAnswer）、模型给了正文但 JSON 半截（decode 失败，未包 errPermanent）。
// 不重问：4xx 鉴权/参数错误、服务端明确 error、status 非 completed —— 这些换个时间
// 问同一问也是同一结局，连问三次只会拖长决策链并多花钱。
func retryable(err error) bool {
	return err != nil && !errors.Is(err, errPermanent)
}

// isCtxErr 超时/取消是"评审没问出来"，不是数据库或链路故障。
func isCtxErr(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
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
//
// 写失败行用脱离取消的上下文：超时/全局 deadline 触发时 ctx 已经 Done，
// 直接拿它写库会连"失败行"都落不下去，当日轨迹里就只剩一片空白。
// 5 秒足够一条 SQLite 写入，又不会在库被锁时拖住收尾。
func (r *Reviewer) markFailed(ctx context.Context, a askAsk, code, why string) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), traceWriteTimeout)
	defer cancel()
	return r.save(wctx, model.LLMCall{TradeDate: a.date, TsCode: code, PromptKey: a.spec.Key,
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
