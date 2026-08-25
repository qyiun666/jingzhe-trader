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
	return batchInsert(r.db, basicInsertSQL, "插入基本面失败", len(basics), func(stmt *sqlx.Stmt, i int) error {
		b := basics[i]
		_, err := stmt.Exec(
			b.TsCode, b.TradeDate, b.Close, b.TurnoverRate, b.VolumeRatio,
			b.PE, b.PE_TTM, b.PB, b.PS, b.PS_TTM, b.DV_RATIO,
			b.TotalMV, b.CircMV, b.LimitStatus,
		)
		if err != nil {
			return fmt.Errorf("ts_code=%s date=%s: %w", b.TsCode, b.TradeDate, err)
		}
		return nil
	})
}

// GetByDate 查询某交易日全市场基本面数据
func (r *BasicRepo) GetByDate(tradeDate string) ([]model.DailyBasic, error) {
	query := fmt.Sprintf(`SELECT %s FROM daily_basic WHERE trade_date = ? ORDER BY ts_code ASC`, basicSelectCols)
	var basics []model.DailyBasic
	if err := selectList(r.db, query, &basics, "查询基本面失败", tradeDate); err != nil {
		return nil, err
	}
	return basics, nil
}

// GetByCode 查询指定股票在 [startDate, endDate] 区间内的基本面数据
func (r *BasicRepo) GetByCode(tsCode, startDate, endDate string) ([]model.DailyBasic, error) {
	query := fmt.Sprintf(`SELECT %s FROM daily_basic
		WHERE ts_code = ? AND trade_date >= ? AND trade_date <= ?
		ORDER BY trade_date ASC`, basicSelectCols)
	var basics []model.DailyBasic
	if err := selectList(r.db, query, &basics, "查询基本面失败", tsCode, startDate, endDate); err != nil {
		return nil, err
	}
	return basics, nil
}

// GetMaxTradeDate 获取 daily_basic 中最大的交易日
func (r *BasicRepo) GetMaxTradeDate() (string, error) {
	return maxTableDate(r.db, "daily_basic")
}
