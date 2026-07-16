package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// StockRepo 股票基本信息仓储
type StockRepo struct {
	db *sqlx.DB
}

// NewStockRepo 构造 StockRepo
func NewStockRepo(db *sqlx.DB) *StockRepo {
	return &StockRepo{db: db}
}

const stockInsertSQL = `INSERT OR REPLACE INTO stock_basic
	(ts_code, symbol, name, market, exchange, is_st, list_status, list_date, delist_date)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

const stockSelectCols = `ts_code, symbol, name, market, exchange, is_st, list_status, list_date, delist_date`

// BatchInsert 批量插入股票基本信息(已存在则覆盖)
func (r *StockRepo) BatchInsert(stocks []model.Stock) error {
	if len(stocks) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(stockInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, s := range stocks {
		isST := 0
		if s.IsST {
			isST = 1
		}
		if _, err := stmt.Exec(
			s.TsCode, s.Symbol, s.Name, s.Market, s.Exchange,
			isST, s.ListStatus, s.ListDate, s.DelistDate,
		); err != nil {
			return fmt.Errorf("插入股票信息失败(ts_code=%s): %w", s.TsCode, err)
		}
	}
	return tx.Commit()
}

// GetAll 获取全部股票
func (r *StockRepo) GetAll() ([]model.Stock, error) {
	query := fmt.Sprintf(`SELECT %s FROM stock_basic ORDER BY ts_code ASC`, stockSelectCols)
	var stocks []model.Stock
	if err := r.db.Select(&stocks, query); err != nil {
		return nil, fmt.Errorf("查询股票列表失败: %w", err)
	}
	return stocks, nil
}

// GetByCode 按代码查询股票, 不存在返回 nil, nil
func (r *StockRepo) GetByCode(tsCode string) (*model.Stock, error) {
	query := fmt.Sprintf(`SELECT %s FROM stock_basic WHERE ts_code = ?`, stockSelectCols)
	var s model.Stock
	if err := r.db.Get(&s, query, tsCode); err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询股票失败: %w", err)
	}
	return &s, nil
}
