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
	if len(flows) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(moneyflowInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, f := range flows {
		if _, err := stmt.Exec(
			f.TsCode, f.TradeDate, f.BuyElgAmount, f.SellElgAmount, f.NetMFAmount,
		); err != nil {
			return fmt.Errorf("插入资金流向失败(ts_code=%s date=%s): %w", f.TsCode, f.TradeDate, err)
		}
	}
	return tx.Commit()
}

// GetByDate 查询某交易日全市场资金流向
func (r *MoneyFlowRepo) GetByDate(tradeDate string) ([]model.MoneyFlow, error) {
	query := fmt.Sprintf(`SELECT %s FROM moneyflow WHERE trade_date = ? ORDER BY net_mf_amount DESC`, moneyflowSelectCols)
	var flows []model.MoneyFlow
	if err := r.db.Select(&flows, query, tradeDate); err != nil {
		return nil, fmt.Errorf("查询资金流向失败: %w", err)
	}
	return flows, nil
}

// GetByCode 查询指定股票在 [startDate, endDate] 区间内的资金流向
func (r *MoneyFlowRepo) GetByCode(tsCode, startDate, endDate string) ([]model.MoneyFlow, error) {
	query := fmt.Sprintf(`SELECT %s FROM moneyflow
		WHERE ts_code = ? AND trade_date >= ? AND trade_date <= ?
		ORDER BY trade_date ASC`, moneyflowSelectCols)
	var flows []model.MoneyFlow
	if err := r.db.Select(&flows, query, tsCode, startDate, endDate); err != nil {
		return nil, fmt.Errorf("查询资金流向失败: %w", err)
	}
	return flows, nil
}

// GetMaxTradeDate 获取 moneyflow 中最大的交易日
func (r *MoneyFlowRepo) GetMaxTradeDate() (string, error) {
	var maxDate string
	err := r.db.Get(&maxDate, `SELECT MAX(trade_date) FROM moneyflow`)
	if err != nil {
		return "", fmt.Errorf("查询最大交易日失败: %w", err)
	}
	return maxDate, nil
}
