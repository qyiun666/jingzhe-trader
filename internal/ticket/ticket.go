// Package ticket 指令单状态机、成交回执记账、账户资产推算（ARCHITECTURE §2.8）。
//
// 核心契约：
//   - OrderTicket 是人在回路的唯一载体，回执的唯一锚点（D3）；
//   - 7 个必填字段缺失即拒绝生成（验收 #9）；
//   - 状态流转全部走 Transition，结果就写在指令单行上（非法转移报错）；
//   - 账本（成交列/position/现金）写库失败一律上抛（验收 #11）。
package ticket

import (
	"context"
	"errors"
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
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
// 必填字段：trade_date / ts_code / name / direction / qty(>0) / reason / valid_until，
// 任一缺失返回 ErrRequiredField（验收 #9）。有效期 = 下一交易日 15:00（EOD）。
//
// 不接风控参数：止损价与本单占比按档位随时可重算，单据不复述一份会过期的副本；
// 信号来自哪条规则只写日志（sig.Rule），不占列。
func (s *Service) Create(ctx context.Context, sig model.Signal, qty model.Qty, gear model.Gear, tradeDays []string) (model.OrderTicket, error) {
	if sig.TradeDate == "" || sig.TsCode == "" || sig.Name == "" || !sig.Direction.Valid() ||
		qty <= 0 || sig.Reason == "" {
		return model.OrderTicket{}, fmt.Errorf("%w: trade_date=%q ts_code=%q name=%q direction=%q qty=%d reason长度=%d",
			ErrRequiredField, sig.TradeDate, sig.TsCode, sig.Name, sig.Direction, qty, len(sig.Reason))
	}
	vu, err := market.EODValidUntil(tradeDays, sig.TradeDate)
	if err != nil {
		return model.OrderTicket{}, err
	}
	validUntil := vu.Format(time.RFC3339)
	// 单据只记"当时决定要做什么"：止损价与本单占比由风控参数按档位现算，不落列复述；
	// 来源（sig.Rule）与创建时间进日志与 reason 文本。
	t := model.OrderTicket{
		TradeDate:  sig.TradeDate,
		TsCode:     sig.TsCode,
		Name:       sig.Name,
		Direction:  sig.Direction,
		Qty:        qty,
		RefPrice:   sig.RefPrice,
		Reason:     sig.Reason,
		Status:     model.TicketDrafted,
		ValidUntil: validUntil,
		Gear:       gear,
	}
	id, err := s.st.TradeRepo().InsertTicket(ctx, t)
	if err != nil {
		return model.OrderTicket{}, err
	}
	t.ID = id
	observability.S().Infow("指令单已生成",
		"ticket_id", id, "date", t.TradeDate, "ts_code", t.TsCode,
		"direction", string(t.Direction), "qty", int64(t.Qty),
		"gear", string(gear), "rule", sig.Rule)
	return t, nil
}

// Transition 指令单状态机唯一入口：校验转移合法性 → 更新状态（结果就在指令单行上）。
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
	if err := s.st.TradeRepo().UpdateTicketStatus(ctx, id, to, reason); err != nil {
		return err
	}
	// 流转结果 = status 一列 + 原因写进 note；"谁在何时"只记日志，不占列。
	observability.S().Infow("指令单状态流转",
		"ticket_id", id, "date", t.TradeDate, "ts_code", t.TsCode,
		"from", string(from), "to", string(to), "actor", actor, "reason", reason)
	return nil
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
