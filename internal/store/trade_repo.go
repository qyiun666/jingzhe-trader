package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// TradeRepo 交易记录(订单/成交/账户快照)仓储
type TradeRepo struct {
	db *sqlx.DB
}

// NewTradeRepo 构造 TradeRepo
func NewTradeRepo(db *sqlx.DB) *TradeRepo {
	return &TradeRepo{db: db}
}

const orderInsertSQL = `INSERT INTO orders
	(run_id, ts_code, side, price, qty, filled_qty, avg_price, status, reason, create_time, update_time)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const timeLayout = "2006-01-02 15:04:05"

// InsertOrder 插入订单记录, 返回自增主键 ID 并回填到 o.ID
func (r *TradeRepo) InsertOrder(o *model.Order) (int64, error) {
	res, err := r.db.Exec(orderInsertSQL,
		o.RunID, o.TsCode, int(o.Side), o.Price, o.Qty, o.FilledQty, o.AvgPrice,
		int(o.Status), o.Reason,
		o.CreateTime.Format(timeLayout), o.UpdateTime.Format(timeLayout),
	)
	if err != nil {
		return 0, fmt.Errorf("插入订单失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取订单ID失败: %w", err)
	}
	o.ID = id
	return id, nil
}

// UpdateOrderStatus 更新订单状态与成交信息
func (r *TradeRepo) UpdateOrderStatus(id int64, status model.OrderStatus, filledQty int, avgPrice float64) error {
	_, err := r.db.Exec(
		`UPDATE orders SET status = ?, filled_qty = ?, avg_price = ?, update_time = ? WHERE id = ?`,
		int(status), filledQty, avgPrice, time.Now().Format(timeLayout), id,
	)
	if err != nil {
		return fmt.Errorf("更新订单失败: %w", err)
	}
	return nil
}

const tradeInsertSQL = `INSERT INTO trades
	(run_id, order_id, ts_code, side, price, qty, amount, commission, stamp_tax,
	 transfer_fee, total_cost, trade_date, trade_time)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const tradeSelectCols = `id, run_id, order_id, ts_code, side, price, qty, amount, commission,
	stamp_tax, transfer_fee, total_cost, trade_date, trade_time`

// InsertTrade 插入成交记录, 返回自增主键 ID 并回填到 t.ID
func (r *TradeRepo) InsertTrade(t *model.Trade) (int64, error) {
	res, err := r.db.Exec(tradeInsertSQL,
		t.RunID, t.OrderID, t.TsCode, int(t.Side), t.Price, t.Qty, t.Amount,
		t.Commission, t.StampTax, t.TransferFee, t.TotalCost, t.TradeDate, t.TradeTime,
	)
	if err != nil {
		return 0, fmt.Errorf("插入成交记录失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("获取成交ID失败: %w", err)
	}
	t.ID = id
	return id, nil
}

// GetTradesByRunID 查询某次运行的全部成交记录(按时间升序)
func (r *TradeRepo) GetTradesByRunID(runID string) ([]model.Trade, error) {
	query := fmt.Sprintf(`SELECT %s FROM trades WHERE run_id = ? ORDER BY id ASC`, tradeSelectCols)
	var trades []model.Trade
	if err := r.db.Select(&trades, query, runID); err != nil {
		return nil, fmt.Errorf("查询成交记录失败: %w", err)
	}
	return trades, nil
}

const accountSnapshotInsertSQL = `INSERT INTO account_snapshot
	(run_id, trade_date, total_asset, cash, market_value, pnl, pnl_pct, total_pnl, total_pnl_pct)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`

const accountSnapshotSelectCols = `trade_date, total_asset, cash, market_value, pnl, pnl_pct, total_pnl, total_pnl_pct`

// InsertAccountSnapshot 插入账户快照
func (r *TradeRepo) InsertAccountSnapshot(runID string, snap model.AccountSnapshot) error {
	_, err := r.db.Exec(accountSnapshotInsertSQL,
		runID, snap.TradeDate, snap.TotalAsset, snap.Cash, snap.MarketValue,
		snap.PnL, snap.PnLPct, snap.TotalPnL, snap.TotalPnLPct,
	)
	if err != nil {
		return fmt.Errorf("插入账户快照失败: %w", err)
	}
	return nil
}

// GetAccountSnapshotsByRunID 查询某次运行的账户快照(按日期升序)
func (r *TradeRepo) GetAccountSnapshotsByRunID(runID string) ([]model.AccountSnapshot, error) {
	query := fmt.Sprintf(`SELECT %s FROM account_snapshot WHERE run_id = ? ORDER BY trade_date ASC`, accountSnapshotSelectCols)
	var snaps []model.AccountSnapshot
	if err := r.db.Select(&snaps, query, runID); err != nil {
		return nil, fmt.Errorf("查询账户快照失败: %w", err)
	}
	return snaps, nil
}

// GetLatestAccountSnapshot 查询某次运行最近一个交易日的账户快照
func (r *TradeRepo) GetLatestAccountSnapshot(runID string) (*model.AccountSnapshot, error) {
	query := fmt.Sprintf(`SELECT %s FROM account_snapshot WHERE run_id = ? ORDER BY trade_date DESC LIMIT 1`, accountSnapshotSelectCols)
	var snap model.AccountSnapshot
	if err := r.db.Get(&snap, query, runID); err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询账户快照失败: %w", err)
	}
	return &snap, nil
}
