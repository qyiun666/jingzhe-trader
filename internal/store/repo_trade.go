package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// TradeRepo 交易域仓储：order_ticket / fill / position / account_snapshot。
type TradeRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// TradeRepo 返回交易域仓储。
func (s *Store) TradeRepo() *TradeRepo {
	return &TradeRepo{wdb: s.writeDB, rdb: s.readDB}
}

// InsertTicket 插入指令单，返回自增 id。
func (r *TradeRepo) InsertTicket(ctx context.Context, t model.OrderTicket) (int64, error) {
	const q = `INSERT INTO order_ticket
		(trade_date, ts_code, name, direction, qty, ref_price_low, ref_price_high, stop_price, reason,
		 position_pct, urgency, source, status, valid_until, gear, profit_lock, goal_snapshot, signal_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := r.wdb.ExecContext(ctx, q,
		t.TradeDate, t.TsCode, t.Name, string(t.Direction), int64(t.Qty), int64(t.RefPriceLow), int64(t.RefPriceHigh), nullFen(t.StopPrice), t.Reason,
		t.PositionPct, t.Urgency, t.Source, string(t.Status), t.ValidUntil, string(t.Gear), boolToInt(t.ProfitLock), t.GoalSnapshot, nullInt64(t.SignalID), t.CreatedAt, t.UpdatedAt,
	)
	if err != nil {
		return 0, fmt.Errorf("插入指令单失败: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UpdateTicketStatus 更新指令单状态与流转时间。
func (r *TradeRepo) UpdateTicketStatus(ctx context.Context, id int64, status model.TicketStatus, now, issuedAt, closedAt string) error {
	const q = `UPDATE order_ticket SET status=?, updated_at=?, issued_at=CASE WHEN ?<>'' THEN ? ELSE issued_at END, closed_at=CASE WHEN ?<>'' THEN ? ELSE closed_at END WHERE id=?`
	if _, err := r.wdb.ExecContext(ctx, q, string(status), now, issuedAt, issuedAt, closedAt, closedAt, id); err != nil {
		return fmt.Errorf("更新指令单状态 %d 失败: %w", id, err)
	}
	return nil
}

// ListActiveTickets 读取指定交易日活跃指令单（drafted/issued）。
func (r *TradeRepo) ListActiveTickets(ctx context.Context, tradeDate string) ([]model.OrderTicket, error) {
	var ts []model.OrderTicket
	if err := r.rdb.SelectContext(ctx, &ts,
		`SELECT `+ticketColumns+` FROM order_ticket WHERE trade_date=? AND status IN ('drafted','issued') ORDER BY id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取活跃指令单 %s 失败: %w", tradeDate, err)
	}
	return ts, nil
}

// ListTickets 读取指定交易日的指令单（status 为空表示全部）。
func (r *TradeRepo) ListTickets(ctx context.Context, tradeDate, status string) ([]model.OrderTicket, error) {
	q := `SELECT ` + ticketColumns + ` FROM order_ticket WHERE trade_date=?`
	args := []interface{}{tradeDate}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	q += " ORDER BY id"
	var ts []model.OrderTicket
	if err := r.rdb.SelectContext(ctx, &ts, q, args...); err != nil {
		return nil, fmt.Errorf("读取指令单 %s 失败: %w", tradeDate, err)
	}
	return ts, nil
}

// InsertFill 写入成交（ticket_id 唯一 → 幂等）。
func (r *TradeRepo) InsertFill(ctx context.Context, f model.Fill) error {
	const q = `INSERT INTO fill (ticket_id, ts_code, direction, qty, price, amount, commission, stamp_tax, transfer_fee, total_cost, trade_date, reported_by, reported_at, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ticket_id) DO NOTHING`
	if _, err := r.wdb.ExecContext(ctx, q,
		f.TicketID, f.TsCode, string(f.Direction), int64(f.Qty), int64(f.Price), int64(f.Amount),
		int64(f.Commission), int64(f.StampTax), int64(f.TransferFee), int64(f.TotalCost), f.TradeDate, f.ReportedBy, f.ReportedAt, f.Note,
	); err != nil {
		return fmt.Errorf("写入成交 %d 失败: %w", f.TicketID, err)
	}
	return nil
}

// UpsertPosition 写入/更新持仓（加权成本在业务层计算后传入）。
func (r *TradeRepo) UpsertPosition(ctx context.Context, p model.Position) error {
	const q = `INSERT INTO position (ts_code, total_qty, available_qty, today_bought, cost_price, high_price, first_open_date, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code) DO UPDATE SET
			total_qty=excluded.total_qty, available_qty=excluded.available_qty, today_bought=excluded.today_bought,
			cost_price=excluded.cost_price, high_price=excluded.high_price, first_open_date=excluded.first_open_date, updated_at=excluded.updated_at`
	if _, err := r.wdb.ExecContext(ctx, q,
		p.TsCode, int64(p.TotalQty), int64(p.AvailableQty), int64(p.TodayBought), int64(p.CostPrice), int64(p.HighPrice), p.FirstOpenDate, p.UpdatedAt,
	); err != nil {
		return fmt.Errorf("写入持仓 %s 失败: %w", p.TsCode, err)
	}
	return nil
}

// GetPosition 读取持仓。
func (r *TradeRepo) GetPosition(ctx context.Context, tsCode string) (model.Position, error) {
	var p model.Position
	err := r.rdb.GetContext(ctx, &p,
		`SELECT ts_code, total_qty, available_qty, today_bought, cost_price, high_price, first_open_date, updated_at
		 FROM position WHERE ts_code=?`, tsCode)
	if err != nil {
		return p, fmt.Errorf("读取持仓 %s 失败: %w", tsCode, err)
	}
	return p, nil
}

// UpsertSnapshot 写入账户日终快照。
func (r *TradeRepo) UpsertSnapshot(ctx context.Context, sn model.AccountSnapshot) error {
	const q = `INSERT INTO account_snapshot (trade_date, cash, market_value, total_asset, position_count, gear, profit_lock, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(trade_date) DO UPDATE SET
			cash=excluded.cash, market_value=excluded.market_value, total_asset=excluded.total_asset,
			position_count=excluded.position_count, gear=excluded.gear, profit_lock=excluded.profit_lock, created_at=excluded.created_at`
	if _, err := r.wdb.ExecContext(ctx, q,
		sn.TradeDate, int64(sn.Cash), int64(sn.MarketValue), int64(sn.TotalAsset), sn.PositionCount, string(sn.Gear), boolToInt(sn.ProfitLock), sn.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入账户快照 %s 失败: %w", sn.TradeDate, err)
	}
	return nil
}

// LatestSnapshot 读取最近一个交易日快照（季初基准回退用）。
func (r *TradeRepo) LatestSnapshot(ctx context.Context) (model.AccountSnapshot, error) {
	var sn model.AccountSnapshot
	err := r.rdb.GetContext(ctx, &sn,
		`SELECT trade_date, cash, market_value, total_asset, position_count, gear, profit_lock, created_at
		 FROM account_snapshot ORDER BY trade_date DESC LIMIT 1`)
	if err != nil {
		return sn, fmt.Errorf("读取最近快照失败: %w", err)
	}
	return sn, nil
}

// HasSnapshot 当日是否已有账户快照（自检/快照幂等判定）。
func (r *TradeRepo) HasSnapshot(ctx context.Context, tradeDate string) (bool, error) {
	var n int
	err := r.rdb.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM account_snapshot WHERE trade_date=?`, tradeDate)
	if err != nil {
		return false, fmt.Errorf("查询快照 %s 失败: %w", tradeDate, err)
	}
	return n > 0, nil
}

// HoldingCodes 返回当前持仓（total_qty > 0）的 ts_code 列表，供新鲜度门禁 #7。
// ListActiveTickets 读取某交易日活跃（drafted/issued）指令单。
// （原方法保留于下方；此处为 Batch 4 新增的逾期扫描查询。）
func (r *TradeRepo) ListExpiredIssued(ctx context.Context, nowRFC3339 string) ([]model.OrderTicket, error) {
	var ts []model.OrderTicket
	if err := r.rdb.SelectContext(ctx, &ts,
		`SELECT id, trade_date, ts_code, name, direction, qty, ref_price_low, ref_price_high,
		 COALESCE(stop_price,0) AS stop_price, reason, COALESCE(position_pct,0) AS position_pct, urgency, source,
		 status, valid_until, gear, profit_lock, COALESCE(goal_snapshot,'') AS goal_snapshot,
		 COALESCE(signal_id,0) AS signal_id, COALESCE(skip_reason,'') AS skip_reason,
		 created_at, updated_at, COALESCE(issued_at,'') AS issued_at, COALESCE(closed_at,'') AS closed_at
		 FROM order_ticket WHERE status='issued' AND valid_until < ? ORDER BY id`, nowRFC3339); err != nil {
		return nil, fmt.Errorf("读取逾期指令单失败: %w", err)
	}
	return ts, nil
}

func (r *TradeRepo) HoldingCodes(ctx context.Context) ([]string, error) {
	var codes []string
	if err := r.rdb.SelectContext(ctx, &codes,
		"SELECT ts_code FROM position WHERE total_qty > 0 ORDER BY ts_code"); err != nil {
		return nil, fmt.Errorf("读取持仓代码失败: %w", err)
	}
	return codes, nil
}

// ListPositions 读取全部持仓（含 total_qty=0 的历史行，调用方自行过滤）。
func (r *TradeRepo) ListPositions(ctx context.Context) ([]model.Position, error) {
	var ps []model.Position
	const q = `SELECT ts_code, total_qty, available_qty, today_bought, cost_price, high_price, first_open_date, updated_at
		FROM position ORDER BY ts_code`
	if err := r.rdb.SelectContext(ctx, &ps, q); err != nil {
		return nil, fmt.Errorf("读取全部持仓失败: %w", err)
	}
	return ps, nil
}

// ticketColumns 指令单读取列（可空列 COALESCE 兜底，避免 NULL 扫描进 string/int64 报错）。
const ticketColumns = `id, trade_date, ts_code, name, direction, qty, ref_price_low, ref_price_high, stop_price, reason,
	 position_pct, urgency, source, status, valid_until, gear, profit_lock,
	 COALESCE(goal_snapshot,'') AS goal_snapshot, COALESCE(signal_id,0) AS signal_id, COALESCE(skip_reason,'') AS skip_reason,
	 created_at, updated_at, COALESCE(issued_at,'') AS issued_at, COALESCE(closed_at,'') AS closed_at`

// GetTicket 读取单张指令单（不存在返回 error）。
func (r *TradeRepo) GetTicket(ctx context.Context, id int64) (model.OrderTicket, error) {
	var t model.OrderTicket
	const q = `SELECT ` + ticketColumns + ` FROM order_ticket WHERE id = ?`
	if err := r.rdb.GetContext(ctx, &t, q, id); err != nil {
		return t, fmt.Errorf("读取指令单 %d 失败: %w", id, err)
	}
	return t, nil
}

// FillTotals 汇总成交：买入总成本（含费）与卖出净到账（金额−费），用于现金推算。
func (r *TradeRepo) FillTotals(ctx context.Context) (buyCost, sellProceeds model.Fen, err error) {
	var rows []struct {
		Direction string     `db:"direction"`
		Total     model.Fen  `db:"total"`
	}
	const q = `SELECT direction, SUM(total_cost) AS total FROM fill GROUP BY direction`
	if err := r.rdb.SelectContext(ctx, &rows, q); err != nil {
		return 0, 0, fmt.Errorf("汇总成交金额失败: %w", err)
	}
	for _, row := range rows {
		switch row.Direction {
		case string(model.DirBuy):
			buyCost = row.Total
		case string(model.DirSell):
			sellProceeds = row.Total
		}
	}
	return buyCost, sellProceeds, nil
}

// FillByTicket 读取指定指令单的成交回执（不存在时 found=false，无 error）。
func (r *TradeRepo) FillByTicket(ctx context.Context, ticketID int64) (model.Fill, bool, error) {
	var f model.Fill
	const q = `SELECT id, ticket_id, ts_code, direction, qty, price, amount, commission, stamp_tax, transfer_fee, total_cost, trade_date, reported_by, reported_at, COALESCE(note,'') AS note
		FROM fill WHERE ticket_id = ?`
	err := r.rdb.GetContext(ctx, &f, q, ticketID)
	if err == nil {
		return f, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return model.Fill{}, false, nil
	}
	return model.Fill{}, false, fmt.Errorf("读取回执 %d 失败: %w", ticketID, err)
}

// CountFills 统计 fill 表行数（回执幂等断言用）。
func (r *TradeRepo) CountFills(ctx context.Context) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM fill"); err != nil {
		return 0, fmt.Errorf("统计成交行数失败: %w", err)
	}
	return n, nil
}
