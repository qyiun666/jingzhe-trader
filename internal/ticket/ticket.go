// Package ticket 指令单状态机、成交回执记账、账户快照（ARCHITECTURE §2.8）。
//
// 核心契约：
//   - OrderTicket 是人在回路的唯一载体，回执的唯一锚点（D3）；
//   - 7 个必填字段缺失即拒绝生成（验收 #9）；
//   - 状态流转全部走 Transition 并落 action_log，非法转移报错；
//   - 账本（fill/position/cash）写库失败一律上抛（验收 #11）。
package ticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
)

// 哨兵错误（§11.1：跨包判等用 errors.Is）。
var (
	ErrIllegalTransition = errors.New("非法指令单状态转移")
	ErrTicketNotFound    = errors.New("指令单不存在")
	ErrRequiredField     = errors.New("指令单必填字段缺失")
)

// Service 指令单服务：生成 + 单一状态机。
type Service struct {
	st *store.Store
}

// NewService 构造指令单服务。
func NewService(st *store.Store) *Service { return &Service{st: st} }

// Create 由信号生成指令单（drafted）。
//
// 7 个必填字段：trade_date / ts_code / name / direction / qty(>0) / reason / valid_until，
// 任一缺失返回 ErrRequiredField（验收 #9）。有效期 = 下一交易日 15:00（EOD）。
func (s *Service) Create(ctx context.Context, sig model.Signal, qty model.Qty, p risk.RiskParams, gear model.Gear, tradeDays []string) (model.OrderTicket, error) {
	if sig.TradeDate == "" || sig.TsCode == "" || sig.Name == "" || !sig.Direction.Valid() ||
		qty <= 0 || sig.Reason == "" {
		return model.OrderTicket{}, fmt.Errorf("%w: trade_date=%q ts_code=%q name=%q direction=%q qty=%d reason长度=%d",
			ErrRequiredField, sig.TradeDate, sig.TsCode, sig.Name, sig.Direction, qty, len(sig.Reason))
	}
	now := time.Now().UTC().Format(time.RFC3339)
	validUntil := market.EODValidUntil(tradeDays, sig.TradeDate).Format(time.RFC3339)
	stop := model.Fen(0)
	if sig.Direction == model.DirBuy && p.StopLossPct > 0 {
		stop = model.Fen(float64(sig.RefPrice) * (1 - p.StopLossPct))
	}
	amount := sig.RefPrice.Mul(qty)
	pct := 0.0
	if p.TotalAsset > 0 {
		pct = float64(amount) / float64(p.TotalAsset)
	}
	t := model.OrderTicket{
		TradeDate:    sig.TradeDate,
		TsCode:       sig.TsCode,
		Name:         sig.Name,
		Direction:    sig.Direction,
		Qty:          qty,
		RefPriceLow:  sig.RefPrice,
		RefPriceHigh: sig.RefPrice,
		StopPrice:    stop,
		Reason:       sig.Reason,
		PositionPct:  pct,
		Urgency:      "normal",
		Source:       "signal:" + sig.Rule,
		Status:       model.TicketDrafted,
		ValidUntil:   validUntil,
		Gear:         gear,
		ProfitLock:   p.MaxTotalPositionPct <= 0.63, // 锁利叠加特征（G1 锁利 = 0.63），仅作快照记录
		SignalID:     sig.ID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	id, err := s.st.TradeRepo().InsertTicket(ctx, t)
	if err != nil {
		return model.OrderTicket{}, err
	}
	t.ID = id
	return t, nil
}

// Transition 指令单状态机唯一入口：校验转移合法性 → 更新状态 → 落 action_log。
// 非法转移返回 ErrIllegalTransition；单据不存在返回 ErrTicketNotFound。
func (s *Service) Transition(ctx context.Context, id int64, to model.TicketStatus, actor, reason string) error {
	t, err := s.st.TradeRepo().GetTicket(ctx, id)
	if err != nil {
		return fmt.Errorf("%w: id=%d", ErrTicketNotFound, id)
	}
	from := t.Status
	if !from.CanTransition(to) {
		return fmt.Errorf("%w: %s → %s (ticket %d)", ErrIllegalTransition, from, to, id)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var issuedAt, closedAt string
	if to == model.TicketIssued {
		issuedAt = now
	}
	if to.IsTerminal() {
		closedAt = now
	}
	if err := s.st.TradeRepo().UpdateTicketStatus(ctx, id, to, now, issuedAt, closedAt); err != nil {
		return err
	}
	return s.st.OpsRepo().InsertActionLog(ctx, model.ActionLog{
		TradeDate:   t.TradeDate,
		Actor:       actor,
		ObjectType:  "order_ticket",
		ObjectID:    fmt.Sprintf("%d", id),
		Action:      "transition",
		BeforeValue: string(from),
		AfterValue:  string(to),
		Reason:      reason,
		CreatedAt:   now,
	})
}

// IssueAll 将指定交易日全部 drafted 指令单置为 issued（次日指令邮件前置步骤）。
// 返回流转的单据数。
func (s *Service) IssueAll(ctx context.Context, tradeDate string) (int, error) {
	tickets, err := s.st.TradeRepo().ListActiveTickets(ctx, tradeDate)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tickets {
		if t.Status != model.TicketDrafted {
			continue
		}
		if err := s.Transition(ctx, t.ID, model.TicketIssued, "system", "EOD 指令下发"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
