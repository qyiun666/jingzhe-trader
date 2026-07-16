package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// PositionRepo 持仓快照仓储
// 负责 position_snapshot 表的写入与查询
type PositionRepo struct {
	db *sqlx.DB
}

// NewPositionRepo 构造 PositionRepo
func NewPositionRepo(db *sqlx.DB) *PositionRepo {
	return &PositionRepo{db: db}
}

const positionSnapshotInsertSQL = `INSERT INTO position_snapshot
	(run_id, trade_date, ts_code, total_qty, available_qty, cost_price, market_price, market_value, floating_pnl)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

// 与 model.Position 的 db 标签对应的列(不含 today_bought / floating_pnl_pct, 这两个字段不入库)
const positionSelectCols = `ts_code, total_qty, available_qty, cost_price, market_price, market_value, floating_pnl`

// InsertPositionSnapshot 插入某只股票的持仓快照
func (r *PositionRepo) InsertPositionSnapshot(runID, tradeDate string, pos model.Position) error {
	_, err := r.db.Exec(positionSnapshotInsertSQL,
		runID, tradeDate, pos.TsCode, pos.TotalQty, pos.AvailableQty,
		pos.CostPrice, pos.MarketPrice, pos.MarketValue, pos.FloatingPnL,
	)
	if err != nil {
		return fmt.Errorf("插入持仓快照失败: %w", err)
	}
	return nil
}

// BatchInsertPositionSnapshot 批量插入某交易日的持仓快照(使用事务)
func (r *PositionRepo) BatchInsertPositionSnapshot(runID, tradeDate string, positions []model.Position) error {
	if len(positions) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(positionSnapshotInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, p := range positions {
		if _, err := stmt.Exec(
			runID, tradeDate, p.TsCode, p.TotalQty, p.AvailableQty,
			p.CostPrice, p.MarketPrice, p.MarketValue, p.FloatingPnL,
		); err != nil {
			return fmt.Errorf("插入持仓快照失败(ts_code=%s): %w", p.TsCode, err)
		}
	}
	return tx.Commit()
}

// GetPositionsByRunID 查询某次运行的全部持仓快照
func (r *PositionRepo) GetPositionsByRunID(runID string) ([]model.Position, error) {
	query := fmt.Sprintf(`SELECT %s FROM position_snapshot WHERE run_id = ? ORDER BY ts_code ASC`, positionSelectCols)
	var positions []model.Position
	if err := r.db.Select(&positions, query, runID); err != nil {
		return nil, fmt.Errorf("查询持仓快照失败: %w", err)
	}
	return positions, nil
}

// GetPositionsByDate 查询某次运行指定交易日的持仓快照
func (r *PositionRepo) GetPositionsByDate(runID, tradeDate string) ([]model.Position, error) {
	query := fmt.Sprintf(`SELECT %s FROM position_snapshot
		WHERE run_id = ? AND trade_date = ? ORDER BY ts_code ASC`, positionSelectCols)
	var positions []model.Position
	if err := r.db.Select(&positions, query, runID, tradeDate); err != nil {
		return nil, fmt.Errorf("查询持仓快照失败: %w", err)
	}
	return positions, nil
}
