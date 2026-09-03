package store

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// FinaRepo 财务域仓储：fina_indicator（慢路径）+ fina_sync_state + fina_sync_item（断点续传）。
type FinaRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// FinaRepo 返回财务域仓储。
func (s *Store) FinaRepo() *FinaRepo {
	return &FinaRepo{wdb: s.writeDB, rdb: s.readDB}
}

// UpsertFinaIndicator 写入财务指标（按 end_date 幂等）。
func (r *FinaRepo) UpsertFinaIndicator(ctx context.Context, f model.FinaIndicator) error {
	const q = `INSERT INTO fina_indicator (ts_code, end_date, ann_date, eps, roe, roe_dt, grossprofit_margin, netprofit_margin, debt_to_assets, netprofit_yoy, or_yoy, bps)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code, end_date) DO UPDATE SET
			ann_date=excluded.ann_date, eps=excluded.eps, roe=excluded.roe, roe_dt=excluded.roe_dt,
			grossprofit_margin=excluded.grossprofit_margin, netprofit_margin=excluded.netprofit_margin,
			debt_to_assets=excluded.debt_to_assets, netprofit_yoy=excluded.netprofit_yoy, or_yoy=excluded.or_yoy, bps=excluded.bps`
	if _, err := r.wdb.ExecContext(ctx, q,
		f.TsCode, f.EndDate, f.AnnDate, f.EPS, f.ROE, f.ROEDt, f.GrossProfitMargin, f.NetprofitMargin,
		f.DebtToAssets, f.NetprofitYoy, f.OrYoy, f.BPS,
	); err != nil {
		return fmt.Errorf("写入财务指标 %s/%s 失败: %w", f.TsCode, f.EndDate, err)
	}
	return nil
}

// FinaSyncState 慢路径断点续传状态行。
type FinaSyncState struct {
	ID           int    `db:"id"`
	Status       string `db:"status"`
	CursorTsCode string `db:"cursor_ts_code"`
	Total        int    `db:"total"`
	Done         int    `db:"done"`
	Failed       int    `db:"failed"`
	StartedAt    string `db:"started_at"`
	FinishedAt   string `db:"finished_at"`
}

// GetSyncState 读取断点续传状态（单行 id=1）。
func (r *FinaRepo) GetSyncState(ctx context.Context) (FinaSyncState, error) {
	var st FinaSyncState
	err := r.rdb.GetContext(ctx, &st, "SELECT id, status, cursor_ts_code, total, done, failed, started_at, finished_at FROM fina_sync_state WHERE id = 1")
	if err != nil {
		return st, fmt.Errorf("读取财务同步状态失败: %w", err)
	}
	return st, nil
}

// UpsertSyncState 写入/更新断点续传状态。
func (r *FinaRepo) UpsertSyncState(ctx context.Context, st FinaSyncState) error {
	const q = `INSERT INTO fina_sync_state (id, status, cursor_ts_code, total, done, failed, started_at, finished_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status, cursor_ts_code=excluded.cursor_ts_code, total=excluded.total,
			done=excluded.done, failed=excluded.failed, started_at=excluded.started_at, finished_at=excluded.finished_at`
	if _, err := r.wdb.ExecContext(ctx, q, st.Status, st.CursorTsCode, st.Total, st.Done, st.Failed, st.StartedAt, st.FinishedAt); err != nil {
		return fmt.Errorf("写入财务同步状态失败: %w", err)
	}
	return nil
}

// UpsertSyncItem 写入单只股票的同步进度（断点续传）。
func (r *FinaRepo) UpsertSyncItem(ctx context.Context, tsCode, status, lastErr, updatedAt string, attempts int, lastEndDate string) error {
	const q = `INSERT INTO fina_sync_item (ts_code, last_sync_end_date, status, attempts, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code) DO UPDATE SET
			last_sync_end_date=excluded.last_sync_end_date, status=excluded.status,
			attempts=excluded.attempts, last_error=excluded.last_error, updated_at=excluded.updated_at`
	if _, err := r.wdb.ExecContext(ctx, q, tsCode, lastEndDate, status, attempts, lastErr, updatedAt); err != nil {
		return fmt.Errorf("写入财务同步项 %s 失败: %w", tsCode, err)
	}
	return nil
}

// FinaIndicatorsAsOf 按 point-in-time 读取某只股票的财务指标：
// 仅返回 ann_date <= asOf 的报告，杜绝前视偏差（无未来财报泄露）。
func (r *FinaRepo) FinaIndicatorsAsOf(ctx context.Context, tsCode, asOf string) ([]model.FinaIndicator, error) {
	rows := []model.FinaIndicator{}
	const q = `SELECT ts_code, end_date, ann_date, eps, roe, roe_dt, grossprofit_margin,
		netprofit_margin, debt_to_assets, netprofit_yoy, or_yoy, bps
		FROM fina_indicator WHERE ts_code = ? AND ann_date <= ? ORDER BY end_date DESC`
	if err := r.rdb.SelectContext(ctx, &rows, q, tsCode, asOf); err != nil {
		return nil, fmt.Errorf("读取财务指标 %s（截至 %s）失败: %w", tsCode, asOf, err)
	}
	return rows, nil
}

// DoneCodes 返回已完成（status='done'）同步的股票代码集合，供慢路径续传跳过。
func (r *FinaRepo) DoneCodes(ctx context.Context) (map[string]bool, error) {
	var codes []string
	if err := r.rdb.SelectContext(ctx, &codes,
		"SELECT ts_code FROM fina_sync_item WHERE status = 'done'"); err != nil {
		return nil, fmt.Errorf("读取已完成财务同步项失败: %w", err)
	}
	m := make(map[string]bool, len(codes))
	for _, c := range codes {
		m[c] = true
	}
	return m, nil
}
