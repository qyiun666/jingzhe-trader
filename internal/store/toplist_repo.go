package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// TopListRepo 龙虎榜数据仓储
type TopListRepo struct {
	db *sqlx.DB
}

// NewTopListRepo 构造 TopListRepo
func NewTopListRepo(db *sqlx.DB) *TopListRepo {
	return &TopListRepo{db: db}
}

const toplistInsertSQL = `INSERT OR REPLACE INTO top_list
	(ts_code, trade_date, name, close, pct_change, turnover_rate, amount, net_amount, buy_amount, sell_amount)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const toplistSelectCols = `ts_code, trade_date, name, close, pct_change, turnover_rate, amount, net_amount, buy_amount, sell_amount`

// BatchInsert 批量插入龙虎榜数据(已存在则覆盖)
func (r *TopListRepo) BatchInsert(list []model.TopList) error {
	return batchInsert(r.db, toplistInsertSQL, "插入龙虎榜失败", len(list), func(stmt *sqlx.Stmt, i int) error {
		t := list[i]
		_, err := stmt.Exec(
			t.TsCode, t.TradeDate, t.Name, t.Close, t.PctChange,
			t.TurnoverRate, t.Amount, t.NetAmount, t.BuyAmount, t.SellAmount,
		)
		if err != nil {
			return fmt.Errorf("ts_code=%s date=%s: %w", t.TsCode, t.TradeDate, err)
		}
		return nil
	})
}

// GetByDate 查询某交易日全市场龙虎榜数据
func (r *TopListRepo) GetByDate(tradeDate string) ([]model.TopList, error) {
	query := fmt.Sprintf(`SELECT %s FROM top_list WHERE trade_date = ? ORDER BY net_amount DESC`, toplistSelectCols)
	var list []model.TopList
	if err := selectList(r.db, query, &list, "查询龙虎榜失败", tradeDate); err != nil {
		return nil, err
	}
	return list, nil
}

// GetByCode 查询指定股票在 [startDate, endDate] 区间内的龙虎榜数据
func (r *TopListRepo) GetByCode(tsCode, startDate, endDate string) ([]model.TopList, error) {
	query := fmt.Sprintf(`SELECT %s FROM top_list
		WHERE ts_code = ? AND trade_date >= ? AND trade_date <= ?
		ORDER BY trade_date ASC`, toplistSelectCols)
	var list []model.TopList
	if err := selectList(r.db, query, &list, "查询龙虎榜失败", tsCode, startDate, endDate); err != nil {
		return nil, err
	}
	return list, nil
}

// GetMaxTradeDate 获取 top_list 中最大的交易日
func (r *TopListRepo) GetMaxTradeDate() (string, error) {
	return maxTableDate(r.db, "top_list")
}
