package ticket

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// Ledger 成交回执记账：加权成本、可卖量（T+1）、现金推算、组合同步、本金 write-once。
//
// 核心契约（验收 #10/#11/#12/#14）：
//   - fill.ticket_id 唯一约束 → 同一单据重复回执天然幂等（第二次零写入）；
//   - fill + position + ticket 状态在同一事务内，任一失败整体回滚并上抛；
//   - 现金不单独落库，由本金与成交历史推算（cash = 本金 − Σ买入总成本 + Σ卖出净到账）。
type Ledger struct {
	st             *store.Store
	cost           market.CostParams
	initialCapital model.Fen // 进程内已知的本金（config account.initial_capital 生效值）
}

// NewLedger 构造账本。initialCapital 为 config 中 account.initial_capital 的生效值（分）。
func NewLedger(st *store.Store, cost market.CostParams, initialCapital model.Fen) *Ledger {
	return &Ledger{st: st, cost: cost, initialCapital: initialCapital}
}

// FillRequest 回执请求（MCP report_fill 的输入）。
type FillRequest struct {
	TicketID int64     // 必填：回执锚点
	TsCode   string    // 校验用：须与单据一致
	Qty      model.Qty // 实际成交量
	Price    model.Fen // 实际成交价（分）
	Note     string
	Actor    string
}

// FillResult 回执结果。Duplicate=true 表示该单据已回执过（幂等命中，账本零改动）。
type FillResult struct {
	Fill      model.Fill
	Duplicate bool
}

// ReportFill 记录成交回执：fill + position + ticket 状态单事务落库。
// 写库失败一律上抛，账本不变（验收 #11）。
func (l *Ledger) ReportFill(ctx context.Context, req FillRequest) (FillResult, error) {
	if req.TicketID <= 0 {
		return FillResult{}, fmt.Errorf("回执失败: ticket_id 必填")
	}
	if req.Qty <= 0 || req.Price <= 0 {
		return FillResult{}, fmt.Errorf("回执失败: 成交量/价非法 (qty=%d price=%d)", req.Qty, int64(req.Price))
	}
	t, err := l.st.TradeRepo().GetTicket(ctx, req.TicketID)
	if err != nil {
		return FillResult{}, fmt.Errorf("%w: ticket_id=%d: %v", ErrTicketNotFound, req.TicketID, err)
	}
	// 幂等先行：该单据已有回执 → 直接返回既有成交，账本零改动（验收 #10）
	existing, found, err := l.st.TradeRepo().FillByTicket(ctx, req.TicketID)
	if err != nil {
		return FillResult{}, err
	}
	if found {
		return FillResult{Fill: existing, Duplicate: true}, nil
	}
	if t.Status.IsTerminal() {
		return FillResult{}, fmt.Errorf("回执失败: 指令单 %d 已处于终态 %s", t.ID, t.Status)
	}
	if req.TsCode != "" && req.TsCode != t.TsCode {
		return FillResult{}, fmt.Errorf("回执失败: 代码不匹配 (单据 %s vs 回执 %s)", t.TsCode, req.TsCode)
	}
	isBuy := t.Direction == model.DirBuy
	amount := req.Price.Mul(req.Qty)
	tc := market.CalcTradeCost(amount, isBuy, l.cost.CommissionRate, l.cost.StampTaxRate, l.cost.TransferFeeRate, l.cost.MinCommission)
	now := time.Now().UTC().Format(time.RFC3339)
	actor := req.Actor
	if actor == "" {
		actor = "manual"
	}

	fill := model.Fill{
		TicketID:    t.ID,
		TsCode:      t.TsCode,
		Direction:   t.Direction,
		Qty:         req.Qty,
		Price:       req.Price,
		Amount:      amount,
		Commission:  tc.Commission,
		StampTax:    tc.StampTax,
		TransferFee: tc.TransferFee,
		TotalCost:   tc.TotalCost,
		TradeDate:   t.TradeDate,
		ReportedBy:  actor,
		ReportedAt:  now,
		Note:        req.Note,
	}

	var result FillResult
	err = store.WithTx(ctx, l.st.WriteDB(), func(tx *sqlx.Tx) error {
		// 1) 写 fill（ticket_id 唯一，冲突即重复回执 → 幂等命中零写入）
		res, err := tx.NamedExecContext(ctx,
			`INSERT INTO fill (ticket_id, ts_code, direction, qty, price, amount, commission, stamp_tax, transfer_fee, total_cost, trade_date, reported_by, reported_at, note)
			 VALUES (:ticket_id, :ts_code, :direction, :qty, :price, :amount, :commission, :stamp_tax, :transfer_fee, :total_cost, :trade_date, :reported_by, :reported_at, :note)`,
			fill)
		if err != nil {
			return fmt.Errorf("写入成交回执 %d 失败: %w", t.ID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// 重复回执：幂等命中。读取既有成交返回，账本零改动。
			var existing model.Fill
			if err := tx.GetContext(ctx, &existing, "SELECT * FROM fill WHERE ticket_id = ?", t.ID); err != nil {
				return fmt.Errorf("读取既有回执 %d 失败: %w", t.ID, err)
			}
			result.Fill = existing
			result.Duplicate = true
			return nil
		}
		if id, err := res.LastInsertId(); err == nil {
			fill.ID = id
		}

		// 2) 更新持仓（加权成本 / T+1 可卖量）
		if err := l.applyPosition(ctx, tx, t, req, tc); err != nil {
			return err
		}

		// 3) 指令单状态 → filled
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.ExecContext(ctx,
			"UPDATE order_ticket SET status=?, updated_at=?, closed_at=? WHERE id=?",
			string(model.TicketFilled), now, now, t.ID); err != nil {
			return fmt.Errorf("更新指令单 %d 为 filled 失败: %w", t.ID, err)
		}

		// 4) 审计
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO action_log (trade_date, actor, object_type, object_id, action, before_value, after_value, reason, created_at)
			 VALUES (?, ?, 'order_ticket', ?, 'fill', ?, ?, ?, ?)`,
			t.TradeDate, actor, fmt.Sprintf("%d", t.ID),
			string(model.TicketIssued), string(model.TicketFilled),
			fmt.Sprintf("回执 %d 股 @ %s 分", int64(req.Qty), req.Price), now); err != nil {
			return fmt.Errorf("写入回执审计日志失败: %w", err)
		}

		result.Fill = fill
		return nil
	})
	if err != nil {
		return FillResult{}, err // 账本失败上抛，禁止降级继续（§11.1）
	}
	return result, nil
}

// applyPosition 事务内更新持仓：买入加权成本 + T+1 占用；卖出减仓。
func (l *Ledger) applyPosition(ctx context.Context, tx *sqlx.Tx, t model.OrderTicket, req FillRequest, tc market.TradeCost) error {
	var pos model.Position
	err := tx.GetContext(ctx, &pos,
		"SELECT ts_code, total_qty, available_qty, today_bought, cost_price, high_price, first_open_date, updated_at FROM position WHERE ts_code=?",
		t.TsCode)
	now := time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		pos = model.Position{TsCode: t.TsCode} // 新建仓
	}
	switch t.Direction {
	case model.DirBuy:
		oldCost := pos.CostPrice.Mul(pos.TotalQty)
		newTotal := pos.TotalQty.Add(req.Qty)
		pos.CostPrice = oldCost.Add(tc.TotalCost).DivQty(newTotal) // (旧成本×旧量 + 成交额 + 费用) / 新量
		pos.TotalQty = newTotal
		pos.TodayBought = pos.TodayBought.Add(req.Qty) // T+1：available_qty 不增
		if req.Price > pos.HighPrice {
			pos.HighPrice = req.Price
		}
		if pos.FirstOpenDate == "" {
			pos.FirstOpenDate = t.TradeDate
		}
	case model.DirSell:
		if pos.TotalQty < req.Qty {
			return fmt.Errorf("回执失败: 卖出 %d 股超过持仓 %d 股 (%s)", int64(req.Qty), int64(pos.TotalQty), t.TsCode)
		}
		pos.TotalQty = pos.TotalQty.Sub(req.Qty)
		pos.AvailableQty = pos.AvailableQty.Sub(req.Qty)
		if pos.AvailableQty < 0 {
			pos.AvailableQty = 0
		}
	}
	pos.UpdatedAt = now
	const q = `INSERT INTO position (ts_code, total_qty, available_qty, today_bought, cost_price, high_price, first_open_date, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code) DO UPDATE SET
			total_qty=excluded.total_qty, available_qty=excluded.available_qty, today_bought=excluded.today_bought,
			cost_price=excluded.cost_price, high_price=excluded.high_price, first_open_date=excluded.first_open_date, updated_at=excluded.updated_at`
	if _, err := tx.ExecContext(ctx, q,
		pos.TsCode, int64(pos.TotalQty), int64(pos.AvailableQty), int64(pos.TodayBought),
		int64(pos.CostPrice), int64(pos.HighPrice), pos.FirstOpenDate, pos.UpdatedAt); err != nil {
		return fmt.Errorf("更新持仓 %s 失败: %w", pos.TsCode, err)
	}
	return nil
}

// SettleT1 T+1 结转（每日 09:25）：available_qty += today_bought，today_bought 归零（验收 #12）。
// 返回结转的持仓行数。
func (l *Ledger) SettleT1(ctx context.Context, date string) (int, error) {
	positions, err := l.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	now := time.Now().UTC().Format(time.RFC3339)
	for _, pos := range positions {
		if pos.TodayBought <= 0 {
			continue
		}
		// 结转后今日买入成为可卖：available = T1Available(total, 0) = total
		avail := market.T1Available(pos.TotalQty, 0)
		const q = `UPDATE position SET available_qty=?, today_bought=0, updated_at=? WHERE ts_code=?`
		if _, err := l.st.WriteDB().ExecContext(ctx, q, int64(avail), now, pos.TsCode); err != nil {
			return n, fmt.Errorf("T+1 结转 %s 失败: %w", pos.TsCode, err)
		}
		n++
	}
	return n, nil
}

// Cash 推算可用现金：本金 − Σ买入总成本 + Σ卖出净到账。
// 现金不单独落库（单一数据源原则：成交历史是唯一事实）。不做截断，
// 负值即真实透支信号（正常情况下风控的现金核算会先于透支拦截）。
func (l *Ledger) Cash(ctx context.Context) (model.Fen, error) {
	buyCost, sellProceeds, err := l.st.TradeRepo().FillTotals(ctx)
	if err != nil {
		return 0, err
	}
	return l.initialCapital - buyCost + sellProceeds, nil
}

// PortfolioInput 组合同步输入（MCP sync_portfolio：以券商实际持仓为准校准账本）。
type PortfolioInput struct {
	TsCode       string
	TotalQty     model.Qty
	AvailableQty model.Qty
	TodayBought  model.Qty
	CostPrice    model.Fen
	HighPrice    model.Fen
}

// SyncPortfolio 以券商实际持仓校准账本，并做本金 write-once：
//   - initial_capital 已非零 → 拒绝覆盖本金，落 action_log 拒绝记录（验收 #14），持仓照常同步；
//   - initial_capital 为零 → 写入本金（首次）。
//
// 返回同步的持仓行数与"本金是否被拒绝覆盖"。
func (l *Ledger) SyncPortfolio(ctx context.Context, tradeDate string, capital model.Fen, items []PortfolioInput, actor string) (int, bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if actor == "" {
		actor = "manual"
	}
	// 本金 write-once（config account.initial_capital）
	rejected := false
	repo := config.NewRepo(l.st)
	raw, err := repo.RawAll(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("读取配置失败: %w", err)
	}
	if row, ok := raw["account.initial_capital"]; ok && row.Value != "" && row.Value != "0" {
		capitalRaw := fmt.Sprintf("%d", int64(capital))
		if row.Value != capitalRaw {
			rejected = true
			if err := l.st.OpsRepo().InsertActionLog(ctx, model.ActionLog{
				TradeDate:   tradeDate,
				Actor:       actor,
				ObjectType:  "account",
				ObjectID:    "initial_capital",
				Action:      "rejected_overwrite",
				BeforeValue: row.Value,
				AfterValue:  capitalRaw,
				Reason:      "initial_capital 为 write-once 配置，拒绝覆盖（如需修正请走人工复核流程）",
				CreatedAt:   now,
			}); err != nil {
				return 0, false, fmt.Errorf("写入本金拒绝审计日志失败: %w", err)
			}
		}
	} else if capital > 0 {
		if err := repo.Set(ctx, "account.initial_capital", fmt.Sprintf("%d", int64(capital)), actor); err != nil {
			return 0, false, fmt.Errorf("写入初始本金失败: %w", err)
		}
		l.initialCapital = capital
	}
	// 持仓校准
	n := 0
	for _, it := range items {
		if it.TsCode == "" {
			return n, rejected, fmt.Errorf("组合同步失败: 第 %d 项缺少 ts_code", n+1)
		}
		pos := model.Position{
			TsCode:       it.TsCode,
			TotalQty:     it.TotalQty,
			AvailableQty: it.AvailableQty,
			TodayBought:  it.TodayBought,
			CostPrice:    it.CostPrice,
			HighPrice:    it.HighPrice,
			UpdatedAt:    now,
		}
		if pos.AvailableQty == 0 && pos.TodayBought > 0 {
			pos.AvailableQty = market.T1Available(pos.TotalQty, pos.TodayBought)
		}
		if err := l.st.TradeRepo().UpsertPosition(ctx, pos); err != nil {
			return n, rejected, err
		}
		n++
	}
	return n, rejected, nil
}
