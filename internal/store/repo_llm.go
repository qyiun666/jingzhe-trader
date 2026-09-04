package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// LLMRepo LLM 决策留痕：一条 prompt 对一只票一行，直接落在 run_trace 上。
//
// 原 llm_call 表没有另起一张的理由：它的唯一键 (trade_date, ts_code, prompt_key)
// 就是 run_trace 的 UNIQUE(trade_date, subject)（subject = llm:<标的>:<prompt_key>），
// status 就是 outcome，保留窗口同为 90 天。结论与理由序列化进 detail。
//
// 这一行同时是当日重跑的回答缓存：查到 outcome=ok 即复用，不再花第二次钱。
type LLMRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// LLMRepo 返回 LLM 留痕仓储。
func (s *Store) LLMRepo() *LLMRepo {
	return &LLMRepo{wdb: s.writeDB, rdb: s.readDB}
}

// llmDetail 是 LLM 行的 run_trace.detail 载荷：只放"这一问的答案"。
// 日期、标的、prompt 都已在键上，不在正文里再抄一遍；键名压到单字母，
// 因为这些行每天每只候选要写 5 条。
type llmDetail struct {
	Verdict    string  `json:"v,omitempty"`
	Confidence float64 `json:"c,omitempty"`
	WeightPct  float64 `json:"w,omitempty"`
	Rationale  string  `json:"r,omitempty"`
	Error      string  `json:"e,omitempty"`
}

// GetCall 读取某标的当日某条 prompt 的留痕；没有留痕返回 found=false。
func (r *LLMRepo) GetCall(ctx context.Context, tradeDate, tsCode, promptKey string) (model.LLMCall, bool, error) {
	var t model.RunTrace
	q := `SELECT ` + traceColumns + ` FROM run_trace WHERE trade_date=? AND subject=?`
	if err := r.rdb.GetContext(ctx, &t, q, tradeDate, model.TraceLLM(tsCode, promptKey)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.LLMCall{}, false, nil
		}
		return model.LLMCall{}, false, fmt.Errorf("读取 LLM 留痕 %s %s/%s 失败: %w", tradeDate, tsCode, promptKey, err)
	}
	var d llmDetail
	if t.Detail != "" {
		if err := json.Unmarshal([]byte(t.Detail), &d); err != nil {
			return model.LLMCall{}, false, fmt.Errorf("LLM 留痕 %s %s/%s 正文解不开: %w", tradeDate, tsCode, promptKey, err)
		}
	}
	return model.LLMCall{
		TradeDate: t.TradeDate, TsCode: tsCode, PromptKey: promptKey,
		Verdict: d.Verdict, Confidence: d.Confidence, WeightPct: d.WeightPct,
		Rationale: d.Rationale, Status: t.Outcome, Error: d.Error, CreatedAt: t.At,
	}, true, nil
}

// SaveCall 写入/覆盖一条回答留痕（幂等键 = 交易日 + subject）。
//
// 冲突时覆盖而非报错：上一轮失败的行必须能被本轮成功的答复顶掉，否则当日重跑会
// 永远卡在那条失败记录上；反过来成功行也会被更晚的一问顶掉，缓存不会锁死在旧答案上。
func (r *LLMRepo) SaveCall(ctx context.Context, c model.LLMCall) error {
	d := llmDetail{Verdict: c.Verdict, Confidence: c.Confidence, WeightPct: c.WeightPct,
		Rationale: c.Rationale, Error: c.Error}
	raw, err := json.Marshal(d)
	if err != nil {
		return fmt.Errorf("序列化 LLM 留痕 %s/%s 失败: %w", c.TsCode, c.PromptKey, err)
	}
	tr := TraceRepo{wdb: r.wdb, rdb: r.rdb}
	if err := tr.Write(ctx, model.RunTrace{
		TradeDate: c.TradeDate, Subject: model.TraceLLM(c.TsCode, c.PromptKey),
		Outcome: c.Status, Detail: string(raw), At: c.CreatedAt,
	}); err != nil {
		return fmt.Errorf("写入 LLM 留痕 %s/%s 失败: %w", c.TsCode, c.PromptKey, err)
	}
	return nil
}
