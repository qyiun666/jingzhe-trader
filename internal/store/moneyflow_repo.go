package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// MoneyFlowRepo 资金流向数据仓储
type MoneyFlowRepo struct {
	db *sqlx.DB
}

// NewMoneyFlowRepo 构造 MoneyFlowRepo
func NewMoneyFlowRepo(db *sqlx.DB) *MoneyFlowRepo {
	return &MoneyFlowRepo{db: db}
}

const moneyflowInsertSQL = `INSERT OR REPLACE INTO moneyflow
	(ts_code, trade_date, buy_elg_amount, sell_elg_amount, net_mf_amount)
	VALUES (?, ?, ?, ?, ?)`

const moneyflowSelectCols = `ts_code, trade_date, buy_elg_amount, sell_elg_amount, net_mf_amount`

// BatchInsert 批量插入资金流向数据(已存在则覆盖)
func (r *MoneyFlowRepo) BatchInsert(flows []model.MoneyFlow) error {
	return batchInsert(r.db, moneyflowInsertSQL, "插入资金流向失败", len(flows), func(stmt *sqlx.Stmt, i int) error {
		f := flows[i]
		_, err := stmt.Exec(
			f.TsCode, f.TradeDate, f.BuyElgAmount, f.SellElgAmount, f.NetMFAmount,
		)
		if err != nil {
			return fmt.Errorf("ts_code=%s date=%s: %w", f.TsCode, f.TradeDate, err)
		}
		return nil
	})
}

// GetByCode 查询指定股票在 [startDate, endDate] 区间内的资金流向
func (r *MoneyFlowRepo) GetByCode(tsCode, startDate, endDate string) ([]model.MoneyFlow, error) {
	query := fmt.Sprintf(`SELECT %s FROM moneyflow
		WHERE ts_code = ? AND trade_date >= ? AND trade_date <= ?
		ORDER BY trade_date ASC`, moneyflowSelectCols)
	var flows []model.MoneyFlow
	if err := selectList(r.db, query, &flows, "查询资金流向失败", tsCode, startDate, endDate); err != nil {
		return nil, err
	}
	return flows, nil
}

// GetMaxTradeDate 获取 moneyflow 中最大的交易日
func (r *MoneyFlowRepo) GetMaxTradeDate() (string, error) {
	return maxTableDate(r.db, "moneyflow")
}
