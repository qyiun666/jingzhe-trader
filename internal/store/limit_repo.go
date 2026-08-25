package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// LimitRepo 涨跌停价仓储
type LimitRepo struct {
	db *sqlx.DB
}

// NewLimitRepo 构造 LimitRepo
func NewLimitRepo(db *sqlx.DB) *LimitRepo {
	return &LimitRepo{db: db}
}

const limitInsertSQL = `INSERT OR REPLACE INTO stk_limit
	(ts_code, trade_date, up_limit, down_limit)
	VALUES (?, ?, ?, ?)`

const limitSelectCols = `ts_code, trade_date, up_limit, down_limit`

// BatchInsert 批量插入涨跌停价(已存在则覆盖)
func (r *LimitRepo) BatchInsert(limits []model.StkLimit) error {
	return batchInsert(r.db, limitInsertSQL, "插入涨跌停价失败", len(limits), func(stmt *sqlx.Stmt, i int) error {
		l := limits[i]
		_, err := stmt.Exec(l.TsCode, l.TradeDate, l.UpLimit, l.DownLimit)
		if err != nil {
			return fmt.Errorf("ts_code=%s date=%s: %w", l.TsCode, l.TradeDate, err)
		}
		return nil
	})
}

// GetByDate 查询某交易日全市场涨跌停价
func (r *LimitRepo) GetByDate(tradeDate string) ([]model.StkLimit, error) {
	query := fmt.Sprintf(`SELECT %s FROM stk_limit WHERE trade_date = ? ORDER BY ts_code ASC`, limitSelectCols)
	var limits []model.StkLimit
	if err := selectList(r.db, query, &limits, "查询涨跌停价失败", tradeDate); err != nil {
		return nil, err
	}
	return limits, nil
}

// GetByCode 查询指定股票在 [startDate, endDate] 区间内的涨跌停价
func (r *LimitRepo) GetByCode(tsCode, startDate, endDate string) ([]model.StkLimit, error) {
	query := fmt.Sprintf(`SELECT %s FROM stk_limit
		WHERE ts_code = ? AND trade_date >= ? AND trade_date <= ?
		ORDER BY trade_date ASC`, limitSelectCols)
	var limits []model.StkLimit
	if err := selectList(r.db, query, &limits, "查询涨跌停价失败", tsCode, startDate, endDate); err != nil {
		return nil, err
	}
	return limits, nil
}

// GetByCodeAndDate 查询指定股票某交易日的涨跌停价
func (r *LimitRepo) GetByCodeAndDate(tsCode, tradeDate string) (*model.StkLimit, error) {
	query := fmt.Sprintf(`SELECT %s FROM stk_limit WHERE ts_code = ? AND trade_date = ?`, limitSelectCols)
	var l model.StkLimit
	found, err := getOne(r.db, query, &l, "查询涨跌停价失败", tsCode, tradeDate)
	if err != nil || !found {
		return nil, err
	}
	return &l, nil
}
