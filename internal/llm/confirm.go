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

// RaiseFunc 紧急告警回调（由 notify.AlertService 注入，避免 llm → notify 依赖）。
type RaiseFunc func(ctx context.Context, tradeDate, source, code, title, content string) error

// llmCallStatus 落库状态常量。失败永远是 failed —— 绝不出现"失败但已分析"。
const (
	statusApproved = "approved"
	statusRejected = "rejected"
	statusFailed   = "failed"
)

// Confirmer signal.BuyConfirmer 的 DeepSeek 实现。
//
// 行为约定：
//   - enabled=false：直接放行（PassThrough 语义，系统功能完整，验收 §10.5-13）；
//   - API 失败/响应不可解析：落 llm_call(failed) + LLM_FAILED urgent，
//     放行该候选但记录 rationale="未质证"（不阻断信号主流程，失败显式可见）；
//   - 成功：解析 verdict 落 llm_call(approved/rejected)，按 verdict 放行/否决。
type Confirmer struct {
	client  *Client
	st      *store.Store
	enabled bool
	raise   RaiseFunc
	now     func() time.Time
}

// NewConfirmer 构造 LLM 终审器。raise 可空（测试）。
func NewConfirmer(client *Client, st *store.Store, enabled bool, raise RaiseFunc) *Confirmer {
	return &Confirmer{client: client, st: st, enabled: enabled && client.Enabled(), raise: raise, now: time.Now}
}

// WithClock 注入时钟（测试用）。
func (c *Confirmer) WithClock(f func() time.Time) *Confirmer {
	if f != nil {
		c.now = f
	}
	return c
}

// verdict LLM 结构化输出。
type verdict struct {
	Approve    bool    `json:"approve"`
	Confidence float64 `json:"confidence"`
	Rationale  string  `json:"rationale"`
}

const confirmSystemPrompt = `你是 A 股量化交易系统的买入候选终审官。你只能对给定候选做"通过/否决"的二次质证，
不能新增候选，不能修改任何参数。请从基本面雷点、极端估值、流动性风险三个角度审查。
必须只输出一个 JSON 对象，格式：{"approve": true/false, "confidence": 0到1的小数, "rationale": "一句话理由"}`

// Confirm 实现 signal.BuyConfirmer。
// 返回 error 仅用于记录链路故障（主流程需要继续），信号侧始终得到放行/否决的明确答复。
func (c *Confirmer) Confirm(ctx context.Context, cand signal.BuyCandidate) (bool, error) {
	if !c.enabled || c.client == nil {
		return true, nil // llm.enabled=false：功能完整，直接放行
	}
	r := cand.Result
	tradeDate := r.TradeDate

	user := fmt.Sprintf(
		"候选买入审查请求：\n代码: %s\n名称: %s\n综合分: %.1f（动量 %.0f 质量 %.0f 价值 %.0f 低波 %.0f 流动性 %.0f）\n收盘价: %.2f 元\n流通市值: %.0f 万\nPE(TTM): %.1f  PB: %.1f  换手率: %.1f%%\n请输出 JSON 结论。",
		r.TsCode, cand.Name, r.Score, r.F_Momentum, r.F_Quality, r.F_Value, r.F_LowVol, r.F_Liquidity,
		float64(r.Close)/100, r.CircMvW, r.PETtm, r.PB, r.TurnoverRate)

	rec := model.LLMCall{
		TradeDate: tradeDate,
		TsCode:    r.TsCode,
		CreatedAt: c.now().UTC().Format(time.RFC3339),
	}
	fail := func(err error) (bool, error) {
		rec.Status = statusFailed
		rec.Error = err.Error()
		rec.Rationale = "未质证" // 显式标注：本次未经 LLM 质证，绝不标"已分析"
		if ierr := c.st.LLMRepo().InsertCall(ctx, rec); ierr != nil {
			observability.S().Errorw("落 LLM 失败记录失败", "code", r.TsCode, "err", ierr.Error())
		}
		// 显式失败通道：LLM_FAILED urgent（不静默吞错）；放行候选但带"未质证"标注，
		// 避免单只候选的 LLM 故障阻断当日全部信号生成（§5.1 15:40 分支语义）。
		if c.raise != nil {
			if rerr := c.raise(ctx, tradeDate, "llm", "LLM_FAILED",
				fmt.Sprintf("LLM 终审失败 %s", r.TsCode),
				fmt.Sprintf("候选 %s(%s) 终审调用失败，信号未质证：%v", r.TsCode, cand.Name, err)); rerr != nil {
				observability.S().Errorw("落 LLM_FAILED 告警失败", "code", r.TsCode, "err", rerr.Error())
			}
		}
		return true, nil
	}

	out, err := c.client.Chat(ctx, confirmSystemPrompt, user)
	if err != nil {
		return fail(err)
	}
	var v verdict
	if perr := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); perr != nil {
		return fail(fmt.Errorf("LLM 输出不可解析为 JSON: %w（原文: %s）", perr, truncate(out, 200)))
	}
	rec.Verdict = map[bool]string{true: "approve", false: "reject"}[v.Approve]
	rec.Confidence = v.Confidence
	rec.Rationale = v.Rationale
	rec.Status = statusRejected
	if v.Approve {
		rec.Status = statusApproved
	}
	if ierr := c.st.LLMRepo().InsertCall(ctx, rec); ierr != nil {
		return v.Approve, fmt.Errorf("落 LLM 终审记录失败: %w", ierr)
	}
	observability.S().Infow("LLM 终审完成", "code", r.TsCode, "status", rec.Status, "rationale", v.Rationale)
	return v.Approve, nil
}
