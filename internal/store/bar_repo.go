package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// BarRepo 日线行情仓储
type BarRepo struct {
	db *sqlx.DB
}

// NewBarRepo 构造 BarRepo
func NewBarRepo(db *sqlx.DB) *BarRepo {
	return &BarRepo{db: db}
}

const (
	barInsertSQL = `INSERT OR REPLACE INTO daily_bar
		(ts_code, trade_date, open, high, low, close, pre_close, change, pct_chg, vol, amount, adj_factor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	barSelectCols = `ts_code, trade_date, open, high, low, close, pre_close, change, pct_chg, vol, amount, adj_factor`
)

// BatchInsert 批量插入日线数据(使用事务, 已存在则覆盖)
func (r *BarRepo) BatchInsert(bars []model.Bar) error {
	if len(bars) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(barInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, b := range bars {
		if _, err := stmt.Exec(
			b.TsCode, b.TradeDate, b.Open, b.High, b.Low, b.Close,
			b.PreClose, b.Change, b.PctChg, b.Vol, b.Amount, b.AdjFactor,
		); err != nil {
			return fmt.Errorf("插入日线失败(ts_code=%s date=%s): %w", b.TsCode, b.TradeDate, err)
		}
	}
	return tx.Commit()
}

// GetBars 查询指定股票在 [startDate, endDate] 区间内的日线(按日期升序)
func (r *BarRepo) GetBars(tsCode, startDate, endDate string) ([]model.Bar, error) {
	query := fmt.Sprintf(`SELECT %s FROM daily_bar
		WHERE ts_code = ? AND trade_date >= ? AND trade_date <= ?
		ORDER BY trade_date ASC`, barSelectCols)
	var bars []model.Bar
	if err := r.db.Select(&bars, query, tsCode, startDate, endDate); err != nil {
		return nil, fmt.Errorf("查询日线失败: %w", err)
	}
	return bars, nil
}

// GetBarsByDate 查询某交易日全市场日线
func (r *BarRepo) GetBarsByDate(tradeDate string) ([]model.Bar, error) {
	query := fmt.Sprintf(`SELECT %s FROM daily_bar
		WHERE trade_date = ? ORDER BY ts_code ASC`, barSelectCols)
	var bars []model.Bar
	if err := r.db.Select(&bars, query, tradeDate); err != nil {
		return nil, fmt.Errorf("查询日线失败: %w", err)
	}
	return bars, nil
}

// GetMaxTradeDate 获取 daily_bar 中最大的交易日(无数据返回空字符串)
func (r *BarRepo) GetMaxTradeDate() (string, error) {
	var maxDate string
	err := r.db.Get(&maxDate, `SELECT MAX(trade_date) FROM daily_bar`)
	if err != nil {
		return "", fmt.Errorf("查询最大交易日失败: %w", err)
	}
	return maxDate, nil
}
