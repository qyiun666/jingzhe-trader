package store

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// DecisionRepo 选股与信号域仓储：screen_result / signal。
type DecisionRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// DecisionRepo 返回选股信号域仓储。
func (s *Store) DecisionRepo() *DecisionRepo {
	return &DecisionRepo{wdb: s.writeDB, rdb: s.readDB}
}

// UpsertScreenResult 写入选股结果（幂等）。
func (r *DecisionRepo) UpsertScreenResult(ctx context.Context, sr model.ScreenResult) error {
	const q = `INSERT INTO screen_result (trade_date, ts_code, rank, score, f_momentum, f_quality, f_value, f_lowvol, f_liquidity, close, circ_mv_w, pe_ttm, pb, turnover_rate, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trade_date, ts_code) DO UPDATE SET
			rank=excluded.rank, score=excluded.score, f_momentum=excluded.f_momentum, f_quality=excluded.f_quality,
			f_value=excluded.f_value, f_lowvol=excluded.f_lowvol, f_liquidity=excluded.f_liquidity, close=excluded.close,
			circ_mv_w=excluded.circ_mv_w, pe_ttm=excluded.pe_ttm, pb=excluded.pb, turnover_rate=excluded.turnover_rate, reason=excluded.reason`
	if _, err := r.wdb.ExecContext(ctx, q,
		sr.TradeDate, sr.TsCode, sr.Rank, sr.Score, sr.F_Momentum, sr.F_Quality, sr.F_Value, sr.F_LowVol, sr.F_Liquidity,
		int64(sr.Close), sr.CircMvW, sr.PETtm, sr.PB, sr.TurnoverRate, sr.Reason,
	); err != nil {
		return fmt.Errorf("写入选股结果 %s/%s 失败: %w", sr.TradeDate, sr.TsCode, err)
	}
	return nil
}

// InsertSignal 插入信号（唯一索引 trade_date+ts_code+direction+rule 保证幂等；冲突忽略）。
func (r *DecisionRepo) InsertSignal(ctx context.Context, sig model.Signal) error {
	const q = `INSERT OR IGNORE INTO signal (trade_date, ts_code, name, direction, rule, confidence, ref_price, reason, payload, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.wdb.ExecContext(ctx, q,
		sig.TradeDate, sig.TsCode, sig.Name, string(sig.Direction), sig.Rule, sig.Confidence, int64(sig.RefPrice),
		sig.Reason, sig.Payload, sig.Status, sig.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入信号 %s/%s 失败: %w", sig.TradeDate, sig.TsCode, err)
	}
	return nil
}

// MarkRejected 风控否决留痕（禁静默丢弃，D1）。
func (r *DecisionRepo) MarkRejected(ctx context.Context, id int64, rule, msg string) error {
	const q = `UPDATE signal SET status='rejected', reject_rule=?, reject_msg=? WHERE id=?`
	if _, err := r.wdb.ExecContext(ctx, q, rule, msg, id); err != nil {
		return fmt.Errorf("标记信号否决 %d 失败: %w", id, err)
	}
	return nil
}

// FindSignalID 按唯一键（trade_date+ts_code+direction+rule）定位信号 id；不存在返回 0。
func (r *DecisionRepo) FindSignalID(ctx context.Context, tradeDate, tsCode string, direction model.Direction, rule string) (int64, error) {
	var id int64
	err := r.rdb.GetContext(ctx, &id,
		"SELECT id FROM signal WHERE trade_date=? AND ts_code=? AND direction=? AND rule=?",
		tradeDate, tsCode, string(direction), rule)
	if err != nil {
		return 0, fmt.Errorf("定位信号 %s/%s/%s/%s 失败: %w", tradeDate, tsCode, direction, rule, err)
	}
	return id, nil
}

// CountSignals 统计指定交易日信号行数（幂等断言用）。
func (r *DecisionRepo) CountSignals(ctx context.Context, tradeDate string) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM signal WHERE trade_date = ?", tradeDate); err != nil {
		return 0, fmt.Errorf("统计信号行数失败: %w", err)
	}
	return n, nil
}

// ListSignals 读取指定交易日全部信号。
func (r *DecisionRepo) ListSignals(ctx context.Context, tradeDate string) ([]model.Signal, error) {
	var sigs []model.Signal
	if err := r.rdb.SelectContext(ctx, &sigs,
		`SELECT id, trade_date, ts_code, name, direction, rule, confidence, ref_price, reason, payload, status, reject_rule, reject_msg, created_at
		 FROM signal WHERE trade_date = ? ORDER BY id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取信号 %s 失败: %w", tradeDate, err)
	}
	return sigs, nil
}
