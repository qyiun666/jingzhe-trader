package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// BasicRepo 每日基本面数据仓储
type BasicRepo struct {
	db *sqlx.DB
}

// NewBasicRepo 构造 BasicRepo
func NewBasicRepo(db *sqlx.DB) *BasicRepo {
	return &BasicRepo{db: db}
}

const basicInsertSQL = `INSERT OR REPLACE INTO daily_basic
	(ts_code, trade_date, close, turnover_rate, volume_ratio, pe, pe_ttm, pb, ps, ps_ttm,
	 dv_ratio, total_mv, circ_mv, limit_status)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const basicSelectCols = `ts_code, trade_date, close, turnover_rate, volume_ratio, pe, pe_ttm, pb,
	ps, ps_ttm, dv_ratio, total_mv, circ_mv, limit_status`

// BatchInsert 批量插入每日基本面数据(已存在则覆盖)
func (r *BasicRepo) BatchInsert(basics []model.DailyBasic) error {
	if len(basics) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(basicInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, b := range basics {
		if _, err := stmt.Exec(
			b.TsCode, b.TradeDate, b.Close, b.TurnoverRate, b.VolumeRatio,
			b.PE, b.PE_TTM, b.PB, b.PS, b.PS_TTM, b.DV_RATIO,
			b.TotalMV, b.CircMV, b.LimitStatus,
		); err != nil {
			return fmt.Errorf("插入基本面失败(ts_code=%s date=%s): %w", b.TsCode, b.TradeDate, err)
		}
	}
	return tx.Commit()
}

// GetByDate 查询某交易日全市场基本面数据
func (r *BasicRepo) GetByDate(tradeDate string) ([]model.DailyBasic, error) {
	query := fmt.Sprintf(`SELECT %s FROM daily_basic WHERE trade_date = ? ORDER BY ts_code ASC`, basicSelectCols)
	var basics []model.DailyBasic
	if err := r.db.Select(&basics, query, tradeDate); err != nil {
		return nil, fmt.Errorf("查询基本面失败: %w", err)
	}
	return basics, nil
}

// GetByCode 查询指定股票在 [startDate, endDate] 区间内的基本面数据
func (r *BasicRepo) GetByCode(tsCode, startDate, endDate string) ([]model.DailyBasic, error) {
	query := fmt.Sprintf(`SELECT %s FROM daily_basic
		WHERE ts_code = ? AND trade_date >= ? AND trade_date <= ?
		ORDER BY trade_date ASC`, basicSelectCols)
	var basics []model.DailyBasic
	if err := r.db.Select(&basics, query, tsCode, startDate, endDate); err != nil {
		return nil, fmt.Errorf("查询基本面失败: %w", err)
	}
	return basics, nil
}

// GetMaxTradeDate 获取 daily_basic 中最大的交易日
func (r *BasicRepo) GetMaxTradeDate() (string, error) {
	var maxDate string
	err := r.db.Get(&maxDate, `SELECT MAX(trade_date) FROM daily_basic`)
	if err != nil {
		return "", fmt.Errorf("查询最大交易日失败: %w", err)
	}
	return maxDate, nil
}
