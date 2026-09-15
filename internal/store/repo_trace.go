package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// TraceRepo 轨迹仓储：一件事一天一行，重跑覆盖。
//
// 取代原 job_run / agent_alert / action_log / mail_outbox 四张表 —— 它们合起来回答的
// 只是"中间做成的事，哪些成了、哪些砸了"，那是一行文本，不是一张状态机。
type TraceRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// TraceRepo 返回轨迹仓储。
func (s *Store) TraceRepo() *TraceRepo {
	return &TraceRepo{wdb: s.writeDB, rdb: s.readDB}
}

// traceColumns 统一 COALESCE 可空列，避免 NULL 扫描进 string 失败。
const traceColumns = `id, trade_date, subject, outcome, COALESCE(detail,'') AS detail, at`

// Write 写入/覆盖一条轨迹（按 trade_date+subject 幂等）。
func (r *TraceRepo) Write(ctx context.Context, t model.RunTrace) error {
	const q = `INSERT INTO run_trace (trade_date, subject, outcome, detail, at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(trade_date, subject) DO UPDATE SET
			outcome=excluded.outcome, detail=excluded.detail, at=excluded.at`
	if _, err := r.wdb.ExecContext(ctx, q, t.TradeDate, t.Subject, t.Outcome, t.Detail, t.At); err != nil {
		return fmt.Errorf("写入轨迹 %s 失败: %w", t.Subject, err)
	}
	return nil
}

// HasSucceeded 这件事今天是否已做成（含做成但有已知缺失）。补跑判定：做成过即不重跑。
func (r *TraceRepo) HasSucceeded(ctx context.Context, subject, tradeDate string) (bool, error) {
	var n int
	err := r.rdb.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM run_trace WHERE subject=? AND trade_date=? AND outcome IN (?,?)`,
		subject, tradeDate, model.TraceOK, model.TracePartial)
	if err != nil {
		return false, fmt.Errorf("查询轨迹 %s 完成状态失败: %w", subject, err)
	}
	return n > 0, nil
}

// LastAt 返回这件事今天最近一次留痕时间（RFC3339 UTC），无记录返回空串。调度冷却判定用。
func (r *TraceRepo) LastAt(ctx context.Context, subject, tradeDate string) (string, error) {
	var s string
	err := r.rdb.GetContext(ctx, &s,
		`SELECT COALESCE(MAX(at),'') FROM run_trace WHERE subject=? AND trade_date=?`, subject, tradeDate)
	if err != nil {
		return "", fmt.Errorf("查询轨迹 %s 最近时间失败: %w", subject, err)
	}
	return s, nil
}

// RecentFailAt 返回这件事最近一次失败时间（RFC3339 UTC），无失败记录返回空串。告警去重用。
func (r *TraceRepo) RecentFailAt(ctx context.Context, subject string) (string, error) {
	var s string
	err := r.rdb.GetContext(ctx, &s,
		`SELECT COALESCE(MAX(at),'') FROM run_trace WHERE subject=? AND outcome=?`, subject, model.TraceFail)
	if err != nil {
		return "", fmt.Errorf("查询轨迹 %s 最近失败时间失败: %w", subject, err)
	}
	return s, nil
}

// List 读取某交易日全部轨迹，按时间排序（日报、自检、MCP 核查用）。
func (r *TraceRepo) List(ctx context.Context, tradeDate string) ([]model.RunTrace, error) {
	var ts []model.RunTrace
	if err := r.rdb.SelectContext(ctx, &ts,
		`SELECT `+traceColumns+` FROM run_trace WHERE trade_date=? ORDER BY at, id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取轨迹 %s 失败: %w", tradeDate, err)
	}
	return ts, nil
}

// LatestBySubject 读取 subject 在 beforeInclusive 当日及之前最近的一行（按 trade_date 降序）。
//
// 用途：日报/盘前邮件要引用"最近一次收盘流水线"的结论。当天 16:30 之前（如 09:00 计划邮件）
// 当日的 job:evening_pipeline 行还不存在，只能取上一交易日的；用 <='到今日' 降序取一行
// 同时覆盖两种时点，且不必让调用方自己算"上一交易日"。
// 无记录返回 ok=false。
func (r *TraceRepo) LatestBySubject(ctx context.Context, subject, beforeInclusive string) (model.RunTrace, bool, error) {
	var t model.RunTrace
	err := r.rdb.GetContext(ctx, &t,
		`SELECT `+traceColumns+` FROM run_trace WHERE subject=? AND trade_date<=?
		 ORDER BY trade_date DESC, at DESC, id DESC LIMIT 1`, subject, beforeInclusive)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.RunTrace{}, false, nil
		}
		return model.RunTrace{}, false, fmt.Errorf("读取 %s（≤%s）最近一行失败: %w", subject, beforeInclusive, err)
	}
	return t, true, nil
}

// llmDecisionSubjectLike 决策留痕行的 subject 模式：llm:<标的>:decision。
const llmDecisionSubjectLike = "llm:%:" + llmPromptKeyDecision

// llmPromptKeyDecision 决策 prompt 的键（与 llm.KeyDecision 同值）。
// 这一处刻意用字面量而不 import llm：store 只依赖 model（依赖方向铁律），
// 而改名时由 repo_trace_test.go 的断言兜住两边同步。
const llmPromptKeyDecision = "decision"

// ListLLMDecisions 读取区间内的决策留痕行（llm:<标的>:decision），升序。
//
// 只取决策行不取证据行：归因要回答的是"模型批不批、多自信"，证据行的立场不参与统计。
func (r *TraceRepo) ListLLMDecisions(ctx context.Context, fromDate, toDate string) ([]model.LLMCall, error) {
	var ts []model.RunTrace
	q := `SELECT ` + traceColumns + ` FROM run_trace
		WHERE subject LIKE ? AND trade_date >= ? AND trade_date <= ? ORDER BY trade_date, subject`
	if err := r.rdb.SelectContext(ctx, &ts, q, llmDecisionSubjectLike, fromDate, toDate); err != nil {
		return nil, fmt.Errorf("读取决策留痕 %s~%s 失败: %w", fromDate, toDate, err)
	}
	out := make([]model.LLMCall, 0, len(ts))
	for _, t := range ts {
		code := strings.TrimSuffix(strings.TrimPrefix(t.Subject, "llm:"), ":"+llmPromptKeyDecision)
		if code == "" {
			continue
		}
		var d llmDetail
		if t.Detail != "" {
			if err := json.Unmarshal([]byte(t.Detail), &d); err != nil {
				return nil, fmt.Errorf("决策留痕 %s 正文解不开: %w", t.Subject, err)
			}
		}
		out = append(out, model.LLMCall{
			TradeDate: t.TradeDate, TsCode: code, PromptKey: llmPromptKeyDecision,
			Verdict: d.Verdict, Confidence: d.Confidence, WeightPct: d.WeightPct,
			Rationale: d.Rationale, Status: t.Outcome, Error: d.Error, CreatedAt: t.At,
		})
	}
	return out, nil
}
