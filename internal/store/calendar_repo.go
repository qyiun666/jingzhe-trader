package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// CalendarRepo 交易日历仓储
type CalendarRepo struct {
	db *sqlx.DB
}

// NewCalendarRepo 构造 CalendarRepo
func NewCalendarRepo(db *sqlx.DB) *CalendarRepo {
	return &CalendarRepo{db: db}
}

const calInsertSQL = `INSERT OR REPLACE INTO trade_cal
	(cal_date, is_open, pretrade_date, exchange)
	VALUES (?, ?, ?, ?)`

const calSelectCols = `cal_date, is_open, pretrade_date, exchange`

// BatchInsert 批量插入交易日历(已存在则覆盖)
func (r *CalendarRepo) BatchInsert(cals []model.TradeCal) error {
	if len(cals) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(calInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, c := range cals {
		if _, err := stmt.Exec(c.CalDate, c.IsOpen, c.PreTradeDate, c.PExchange); err != nil {
			return fmt.Errorf("插入交易日历失败(cal_date=%s): %w", c.CalDate, err)
		}
	}
	return tx.Commit()
}

// GetTradeDays 查询 [startDate, endDate] 区间内的交易日(is_open=1), 按日期升序
func (r *CalendarRepo) GetTradeDays(startDate, endDate string) ([]model.TradeCal, error) {
	query := fmt.Sprintf(`SELECT %s FROM trade_cal
		WHERE is_open = 1 AND cal_date >= ? AND cal_date <= ?
		ORDER BY cal_date ASC`, calSelectCols)
	var cals []model.TradeCal
	if err := r.db.Select(&cals, query, startDate, endDate); err != nil {
		return nil, fmt.Errorf("查询交易日失败: %w", err)
	}
	return cals, nil
}

// GetByDate 查询某日的日历记录
func (r *CalendarRepo) GetByDate(date string) (*model.TradeCal, error) {
	query := fmt.Sprintf(`SELECT %s FROM trade_cal WHERE cal_date = ?`, calSelectCols)
	var c model.TradeCal
	if err := r.db.Get(&c, query, date); err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询交易日历失败: %w", err)
	}
	return &c, nil
}

// IsTradeDay 判断指定日期是否为交易日
func (r *CalendarRepo) IsTradeDay(date string) (bool, error) {
	c, err := r.GetByDate(date)
	if err != nil {
		return false, err
	}
	if c == nil {
		return false, nil
	}
	return c.IsOpen == 1, nil
}

// GetPreTradeDate 获取指定日期的上一交易日
// 优先使用日历表中的 pretrade_date; 若无则向上回溯查找最近一个交易日
func (r *CalendarRepo) GetPreTradeDate(date string) (string, error) {
	c, err := r.GetByDate(date)
	if err != nil {
		return "", err
	}
	if c != nil && c.PreTradeDate != "" {
		return c.PreTradeDate, nil
	}
	// 回溯查找最近一个早于 date 的交易日
	var pre string
	err = r.db.Get(&pre, `SELECT MAX(cal_date) FROM trade_cal
		WHERE is_open = 1 AND cal_date < ?`, date)
	if err != nil {
		return "", fmt.Errorf("查询上一交易日失败: %w", err)
	}
	return pre, nil
}

// GetNextTradeDate 获取指定日期之后的下一个交易日
func (r *CalendarRepo) GetNextTradeDate(date string) (string, error) {
	var next string
	err := r.db.Get(&next, `SELECT MIN(cal_date) FROM trade_cal
		WHERE is_open = 1 AND cal_date > ?`, date)
	if err != nil {
		return "", fmt.Errorf("查询下一交易日失败: %w", err)
	}
	return next, nil
}
