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
	return batchInsert(r.db, newShareInsertSQL, "插入新股数据失败", len(shares), func(stmt *sqlx.Stmt, i int) error {
		s := shares[i]
		_, err := stmt.Exec(
			s.TsCode, s.SubCode, s.Name, s.IpoDate, s.IssueDate,
			s.Amount, s.MarketAmount, s.Price, s.PE, s.LimitAmount,
			s.Funds, s.Ballot,
		)
		if err != nil {
			return fmt.Errorf("ts_code=%s: %w", s.TsCode, err)
		}
		return nil
	})
}
