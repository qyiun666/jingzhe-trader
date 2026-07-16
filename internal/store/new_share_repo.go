package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// NewShareRepo 新股申购数据仓储
type NewShareRepo struct {
	db *sqlx.DB
}

// NewNewShareRepo 构造 NewShareRepo
func NewNewShareRepo(db *sqlx.DB) *NewShareRepo {
	return &NewShareRepo{db: db}
}

const newShareInsertSQL = `INSERT OR REPLACE INTO new_shares
	(ts_code, sub_code, name, ipo_date, issue_date, amount, market_amount, price, pe, limit_amount, funds, ballot)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const newShareSelectCols = `ts_code, sub_code, name, ipo_date, issue_date, amount, market_amount, price, pe, limit_amount, funds, ballot`

// BatchInsert 批量插入新股申购数据(已存在则覆盖)
func (r *NewShareRepo) BatchInsert(shares []model.NewShare) error {
	if len(shares) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(newShareInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, s := range shares {
		if _, err := stmt.Exec(
			s.TsCode, s.SubCode, s.Name, s.IpoDate, s.IssueDate,
			s.Amount, s.MarketAmount, s.Price, s.PE, s.LimitAmount,
			s.Funds, s.Ballot,
		); err != nil {
			return fmt.Errorf("插入新股数据失败(ts_code=%s): %w", s.TsCode, err)
		}
	}
	return tx.Commit()
}

// GetRecent 获取最近 n 条新股申购数据(按上市日期倒序)
func (r *NewShareRepo) GetRecent(n int) ([]model.NewShare, error) {
	query := fmt.Sprintf(`SELECT %s FROM new_shares ORDER BY ipo_date DESC LIMIT ?`, newShareSelectCols)
	var shares []model.NewShare
	if err := r.db.Select(&shares, query, n); err != nil {
		return nil, fmt.Errorf("查询新股数据失败: %w", err)
	}
	return shares, nil
}

// GetByDateRange 按日期范围查询新股申购数据
func (r *NewShareRepo) GetByDateRange(start, end string) ([]model.NewShare, error) {
	query := fmt.Sprintf(`SELECT %s FROM new_shares
		WHERE ipo_date >= ? AND ipo_date <= ?
		ORDER BY ipo_date DESC`, newShareSelectCols)
	var shares []model.NewShare
	if err := r.db.Select(&shares, query, start, end); err != nil {
		return nil, fmt.Errorf("查询新股数据失败: %w", err)
	}
	return shares, nil
}
