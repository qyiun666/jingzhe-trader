package store

import (
	"context"
	"fmt"

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
