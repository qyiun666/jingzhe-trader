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
	(ts_code, symbol, name, market, exchange, is_st, list_status, list_date, delist_date, industry)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// industry 列存在 NULL (如部分北交所股票), 用 COALESCE 兜底避免 Scan 报错
const stockSelectCols = `ts_code, symbol, name, market, exchange, is_st, list_status, list_date, delist_date, COALESCE(industry, '') AS industry`

// BatchInsert 批量插入股票基本信息(已存在则覆盖)
func (r *StockRepo) BatchInsert(stocks []model.Stock) error {
	return batchInsert(r.db, stockInsertSQL, "插入股票信息失败", len(stocks), func(stmt *sqlx.Stmt, i int) error {
		s := stocks[i]
		isST := 0
		if s.IsST {
			isST = 1
		}
		_, err := stmt.Exec(
			s.TsCode, s.Symbol, s.Name, s.Market, s.Exchange,
			isST, s.ListStatus, s.ListDate, s.DelistDate, s.Industry,
		)
		if err != nil {
			return fmt.Errorf("ts_code=%s: %w", s.TsCode, err)
		}
		return nil
	})
}

// GetAll 获取全部股票
func (r *StockRepo) GetAll() ([]model.Stock, error) {
	query := fmt.Sprintf(`SELECT %s FROM stock_basic ORDER BY ts_code ASC`, stockSelectCols)
	var stocks []model.Stock
	if err := selectList(r.db, query, &stocks, "查询股票列表失败"); err != nil {
		return nil, err
	}
	return stocks, nil
}

// GetByCode 按代码查询股票, 不存在返回 nil, nil
func (r *StockRepo) GetByCode(tsCode string) (*model.Stock, error) {
	query := fmt.Sprintf(`SELECT %s FROM stock_basic WHERE ts_code = ?`, stockSelectCols)
	var s model.Stock
	found, err := getOne(r.db, query, &s, "查询股票失败", tsCode)
	if err != nil || !found {
		return nil, err
	}
	return &s, nil
}
