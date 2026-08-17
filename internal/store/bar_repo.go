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
	// UPSERT: 行情字段覆盖, adj_factor 在新值为 0(未获取)时保留已有因子, 防止日常同步把回填的因子抹掉
	barInsertSQL = `INSERT INTO daily_bar
		(ts_code, trade_date, open, high, low, close, pre_close, change, pct_chg, vol, amount, adj_factor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET
		open=excluded.open, high=excluded.high, low=excluded.low, close=excluded.close,
		pre_close=excluded.pre_close, change=excluded.change, pct_chg=excluded.pct_chg,
		vol=excluded.vol, amount=excluded.amount,
		adj_factor=COALESCE(NULLIF(excluded.adj_factor, 0), daily_bar.adj_factor)`
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

// UpdateAdjFactors 批量更新指定股票的复权因子, 返回更新行数
func (r *BarRepo) UpdateAdjFactors(tsCode string, factors map[string]float64) (int, error) {
	if len(factors) == 0 {
		return 0, nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Preparex(`UPDATE daily_bar SET adj_factor = ? WHERE ts_code = ? AND trade_date = ?`)
	if err != nil {
		return 0, fmt.Errorf("预编译更新语句失败: %w", err)
	}
	defer stmt.Close()
	updated := 0
	for date, factor := range factors {
		if factor <= 0 {
			continue
		}
		res, err := stmt.Exec(factor, tsCode, date)
		if err != nil {
			return updated, fmt.Errorf("更新复权因子失败(ts_code=%s date=%s): %w", tsCode, date, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated += int(n)
		}
	}
	return updated, tx.Commit()
}

// GetZeroAdjFactorCodes 获取 daily_bar 中存在复权因子缺失(0或NULL)的股票代码
func (r *BarRepo) GetZeroAdjFactorCodes() ([]string, error) {
	var codes []string
	err := r.db.Select(&codes, `SELECT DISTINCT ts_code FROM daily_bar
		WHERE adj_factor IS NULL OR adj_factor = 0`)
	if err != nil {
		return nil, fmt.Errorf("查询缺失复权因子的股票失败: %w", err)
	}
	return codes, nil
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
