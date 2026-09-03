package store

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// GoalRepo 目标域仓储：goal_state / goal_gear_log。
type GoalRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// GoalRepo 返回目标域仓储。
func (s *Store) GoalRepo() *GoalRepo {
	return &GoalRepo{wdb: s.writeDB, rdb: s.readDB}
}

// UpsertGoalState 写入/更新档位状态机单行（id=1）。
func (r *GoalRepo) UpsertGoalState(ctx context.Context, g model.GoalState) error {
	const q = `INSERT INTO goal_state
		(id, quarter, quarter_start, quarter_end, baseline_asset, peak_asset, current_gear, profit_lock,
		 upgrade_streak, last_eval_date, override_gear, override_reason, override_until, pace_policy, pace_confirm_date, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			quarter=excluded.quarter, quarter_start=excluded.quarter_start, quarter_end=excluded.quarter_end,
			baseline_asset=excluded.baseline_asset, peak_asset=excluded.peak_asset, current_gear=excluded.current_gear,
			profit_lock=excluded.profit_lock, upgrade_streak=excluded.upgrade_streak, last_eval_date=excluded.last_eval_date,
			override_gear=excluded.override_gear, override_reason=excluded.override_reason, override_until=excluded.override_until,
			pace_policy=excluded.pace_policy, pace_confirm_date=excluded.pace_confirm_date, updated_at=excluded.updated_at`
	if _, err := r.wdb.ExecContext(ctx, q,
		g.Quarter, g.QuarterStart, g.QuarterEnd, int64(g.BaselineAsset), int64(g.PeakAsset), string(g.CurrentGear), boolToInt(g.ProfitLock),
		g.UpgradeStreak, g.LastEvalDate, g.OverrideGear, g.OverrideReason, g.OverrideUntil, g.PacePolicy, g.PaceConfirmDate, g.UpdatedAt,
	); err != nil {
		return fmt.Errorf("写入档位状态失败: %w", err)
	}
	return nil
}

// GetGoalState 读取档位状态单行。
func (r *GoalRepo) GetGoalState(ctx context.Context) (model.GoalState, error) {
	var g model.GoalState
	err := r.rdb.GetContext(ctx, &g,
		`SELECT quarter, quarter_start, quarter_end, baseline_asset, peak_asset, current_gear, profit_lock,
		 upgrade_streak, last_eval_date, override_gear, override_reason, override_until, pace_policy, pace_confirm_date, updated_at
		 FROM goal_state WHERE id=1`)
	if err != nil {
		return g, fmt.Errorf("读取档位状态失败: %w", err)
	}
	return g, nil
}

// InsertGearLog 插入档位变更日志（可回放）。
func (r *GoalRepo) InsertGearLog(ctx context.Context, l model.GoalGearLog) error {
	const q = `INSERT INTO goal_gear_log
		(trade_date, quarter, from_gear, to_gear, from_lock, to_lock, trigger_rule, progress, budget_consumed, pace_gap, is_manual, reason, params_snapshot, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.wdb.ExecContext(ctx, q,
		l.TradeDate, l.Quarter, string(l.FromGear), string(l.ToGear), boolToInt(l.FromLock), boolToInt(l.ToLock),
		l.TriggerRule, l.Progress, l.BudgetConsumed, l.PaceGap, boolToInt(l.IsManual), l.Reason, l.ParamsSnapshot, l.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入档位变更日志失败: %w", err)
	}
	return nil
}
