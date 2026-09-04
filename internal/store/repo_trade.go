package store

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// TradeRepo 交易域仓储：order_ticket（指令单，含成交回执）/ position（持仓）。
//
// 成交回执直接落在指令单行上（一单最多一回执），不再有独立 fill 表。
// 账户资产（现金/市值/总资产）不落库，由指令单成交列 + position 实时推算，见 ticket.Ledger.Assets。
type TradeRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// TradeRepo 返回交易域仓储。
func (s *Store) TradeRepo() *TradeRepo {
	return &TradeRepo{wdb: s.writeDB, rdb: s.readDB}
}

// ticketColumns 指令单读取列（可空列 COALESCE 兜底，避免 NULL 扫描进 string 报错）。
const ticketColumns = `id, trade_date, ts_code, name, direction, qty, ref_price, reason,
	 status, valid_until, gear,
	 fill_qty, fill_price, total_cost,
     COALESCE(reported_by,'') AS reported_by, COALESCE(reported_at,'') AS reported_at,
	 COALESCE(note,'') AS note`

// InsertTicket 插入指令单，返回自增 id。
func (r *TradeRepo) InsertTicket(ctx context.Context, t model.OrderTicket) (int64, error) {
	const q = `INSERT INTO order_ticket
		(trade_date, ts_code, name, direction, qty, ref_price, reason, status, valid_until, gear)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := r.wdb.ExecContext(ctx, q,
		t.TradeDate, t.TsCode, t.Name, string(t.Direction), int64(t.Qty), int64(t.RefPrice),
		t.Reason, string(t.Status), t.ValidUntil, string(t.Gear),
	)
	if err != nil {
		return 0, fmt.Errorf("插入指令单失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取回指令单 id 失败: %w", err)
	}
	return id, nil
}

// UpdateTicketStatus 更新指令单状态，并把这次流转的原因记进 note 列。
//
// 不写 updated_at：流转发生的时间与人只进服务日志，库里那一列没有读者。
func (r *TradeRepo) UpdateTicketStatus(ctx context.Context, id int64, status model.TicketStatus, note string) error {
	const q = `UPDATE order_ticket SET status=?, note=? WHERE id=?`
	if _, err := r.wdb.ExecContext(ctx, q, string(status), note, id); err != nil {
		return fmt.Errorf("更新指令单状态 %d 失败: %w", id, err)
	}
	return nil
}

// RecordFillTx 事务内把成交回执写回指令单行并置为终态。
// 调用方须先确认该行尚无回执（HasFill），否则视为重复回执直接返回。
func RecordFillTx(ctx context.Context, tx *sqlx.Tx, f model.OrderTicket, status model.TicketStatus) error {
	const q = `UPDATE order_ticket SET status=?, fill_qty=?, fill_price=?, total_cost=?,
		reported_by=?, reported_at=?, note=? WHERE id=?`
	if _, err := tx.ExecContext(ctx, q,
		string(status), int64(f.FillQty), int64(f.FillPrice), int64(f.TotalCost),
		f.ReportedBy, f.ReportedAt, f.Note, f.ID); err != nil {
		return fmt.Errorf("写入指令单 %d 的成交回执失败: %w", f.ID, err)
	}
	return nil
}

// ListActiveTickets 读取指定交易日活跃指令单（drafted/issued）。
func (r *TradeRepo) ListActiveTickets(ctx context.Context, tradeDate string) ([]model.OrderTicket, error) {
	var ts []model.OrderTicket
	q := `SELECT ` + ticketColumns + ` FROM order_ticket WHERE trade_date=? AND status IN ('drafted','issued') ORDER BY id`
	if err := r.rdb.SelectContext(ctx, &ts, q, tradeDate); err != nil {
		return nil, fmt.Errorf("读取活跃指令单 %s 失败: %w", tradeDate, err)
	}
	return ts, nil
}

// ExpireStale 把过了有效期仍未执行的单收成 expired，返回收掉行数。
//
// cutoff 是"现在"的 RFC3339 串（由调用方按交易所时区格式化并传入 —— 库里 valid_until
// 就是这个格式，同偏移下字典序即时间序）。没有这一步，一张没人下的单会永远挂在
// issued 上，而 order_ticket 是唯一的结果表，它必须说真话。
func (r *TradeRepo) ExpireStale(ctx context.Context, cutoff string) (int, error) {
	const q = `UPDATE order_ticket SET status = 'expired'
		WHERE status IN ('drafted','issued') AND valid_until < ?`
	res, err := r.wdb.ExecContext(ctx, q, cutoff)
	if err != nil {
		return 0, fmt.Errorf("过期未执行指令单（cutoff=%s）失败: %w", cutoff, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("统计过期指令单行数失败: %w", err)
	}
	return int(n), nil
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

// GetTicket 读取单张指令单（不存在返回 error）。
func (r *TradeRepo) GetTicket(ctx context.Context, id int64) (model.OrderTicket, error) {
	var t model.OrderTicket
	const q = `SELECT ` + ticketColumns + ` FROM order_ticket WHERE id = ?`
	if err := r.rdb.GetContext(ctx, &t, q, id); err != nil {
		return t, fmt.Errorf("读取指令单 %d 失败: %w", id, err)
	}
	return t, nil
}

// CountFilled 统计已登记回执的指令单数（回执幂等断言用）。
func (r *TradeRepo) CountFilled(ctx context.Context) (int, error) {
	var n int
	q := `SELECT COUNT(*) FROM order_ticket WHERE reported_at IS NOT NULL AND reported_at <> ''`
	if err := r.rdb.GetContext(ctx, &n, q); err != nil {
		return 0, fmt.Errorf("统计回执数失败: %w", err)
	}
	return n, nil
}

// FillTotals 汇总成交：买入总成本（含费）与卖出净到账（金额−费），用于现金推算。
//
// after 非空时只统计该日期**之后**的成交（组合同步锚点之后的变动）；
// 锚点当日及之前的成交视为已含在券商给的现金快照里，不重复计算。空串 = 全部成交。
func (r *TradeRepo) FillTotals(ctx context.Context, after string) (buyCost, sellProceeds model.Fen, err error) {
	var rows []struct {
		Direction string    `db:"direction"`
		Total     model.Fen `db:"total"`
	}
	const q = `SELECT direction, SUM(total_cost) AS total FROM order_ticket
		WHERE trade_date > ? AND reported_at IS NOT NULL AND reported_at <> '' GROUP BY direction`
	if err := r.rdb.SelectContext(ctx, &rows, q, after); err != nil {
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

// positionColumns 持仓读取列。可用数量不在其中：它是 total_qty − today_bought 的结果，
// 由 model.Position.Available() 现算。
const positionColumns = `ts_code, total_qty, today_bought, cost_price, high_price,
	 COALESCE(first_open_date,'') AS first_open_date`

// UpsertPosition 写入/更新持仓（加权成本在业务层计算后传入）。
func (r *TradeRepo) UpsertPosition(ctx context.Context, p model.Position) error {
	if err := WithTx(ctx, r.wdb, func(tx *sqlx.Tx) error { return UpsertPositionTx(ctx, tx, p) }); err != nil {
		return err
	}
	return nil
}

// UpsertPositionTx 事务内写入/更新持仓。回执链路的持仓变更必须与成交写回同事务，
// 因此读写库上那一份实现抽出来复用，避免两处列清单漂开。
func UpsertPositionTx(ctx context.Context, tx *sqlx.Tx, p model.Position) error {
	const q = `INSERT INTO position (ts_code, total_qty, today_bought, cost_price, high_price, first_open_date)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code) DO UPDATE SET
			total_qty=excluded.total_qty, today_bought=excluded.today_bought,
			cost_price=excluded.cost_price, high_price=excluded.high_price,
			first_open_date=excluded.first_open_date`
	if _, err := tx.ExecContext(ctx, q,
		p.TsCode, int64(p.TotalQty), int64(p.TodayBought), int64(p.CostPrice),
		int64(p.HighPrice), p.FirstOpenDate,
	); err != nil {
		return fmt.Errorf("写入持仓 %s 失败: %w", p.TsCode, err)
	}
	return nil
}

// GetPosition 读取持仓。
func (r *TradeRepo) GetPosition(ctx context.Context, tsCode string) (model.Position, error) {
	var p model.Position
	q := `SELECT ` + positionColumns + ` FROM position WHERE ts_code=?`
	if err := r.rdb.GetContext(ctx, &p, q, tsCode); err != nil {
		return p, fmt.Errorf("读取持仓 %s 失败: %w", tsCode, err)
	}
	return p, nil
}

// HoldingCodes 返回当前持仓（total_qty > 0）的 ts_code 列表，供盘中扫描与新鲜度门禁。
func (r *TradeRepo) HoldingCodes(ctx context.Context) ([]string, error) {
	var codes []string
	if err := r.rdb.SelectContext(ctx, &codes,
		"SELECT ts_code FROM position WHERE total_qty > 0 ORDER BY ts_code"); err != nil {
		return nil, fmt.Errorf("读取持仓代码失败: %w", err)
	}
	return codes, nil
}

// LastBuyDates 每个标的最近一笔已成交买单的交易日（无成交记录则不出现在结果里）。
//
// T+1 结转要用它判断"这批 today_bought 是哪一天买的"：只按 today_bought 归零的话，
// 补跑一次当天 morning_plan 就会把当天刚买的股票解锁成可卖。
func (r *TradeRepo) LastBuyDates(ctx context.Context) (map[string]string, error) {
	type row struct {
		TsCode    string `db:"ts_code"`
		TradeDate string `db:"trade_date"`
	}
	var rows []row
	const q = `SELECT ts_code, MAX(trade_date) AS trade_date FROM order_ticket
		WHERE direction = 'buy' AND status = 'filled' GROUP BY ts_code`
	if err := r.rdb.SelectContext(ctx, &rows, q); err != nil {
		return nil, fmt.Errorf("读取最近买入成交日失败: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, x := range rows {
		out[x.TsCode] = x.TradeDate
	}
	return out, nil
}

// ListPositions 读取全部持仓（含 total_qty=0 的清仓行，调用方自行过滤）。
func (r *TradeRepo) ListPositions(ctx context.Context) ([]model.Position, error) {
	var ps []model.Position
	q := `SELECT ` + positionColumns + ` FROM position ORDER BY ts_code`
	if err := r.rdb.SelectContext(ctx, &ps, q); err != nil {
		return nil, fmt.Errorf("读取全部持仓失败: %w", err)
	}
	return ps, nil
}
