package ticket

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// Ledger 成交回执记账：加权成本、可卖量（T+1）、现金推算、组合同步、本金 write-once。
//
// 核心契约（验收 #10/#11/#12/#14）：
//   - 一单最多一回执（成交列就在指令单行上）→ 以 reported_at 非空判重，重复回执天然幂等；
//   - 成交列写回 + 持仓更新在同一事务内，任一失败整体回滚并上抛；
//   - 现金不单独落库，由本金/现金锚点与成交历史推算（cash = 锚点 − Σ买入总成本 + Σ卖出净到账）。
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
	if t.HasFill() {
		return FillResult{Fill: t.FillView(), Duplicate: true}, nil
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

	// 成交回执就是指令单这一行的成交列（一单最多一回执），在其副本上填好后整体写回
	f := t
	f.FillQty = req.Qty
	f.FillPrice = req.Price
	f.TotalCost = tc.TotalCost // 含费合计；金额与三项费用由使用处现算，不落列
	f.ReportedBy = actor
	f.ReportedAt = now
	f.Note = req.Note

	var result FillResult
	err = store.WithTx(ctx, l.st.WriteDB(), func(tx *sqlx.Tx) error {
		// 1) 成交列 + 终态状态一次写回指令单行
		if err := store.RecordFillTx(ctx, tx, f, model.TicketFilled); err != nil {
			return err
		}
		// 2) 更新持仓（加权成本 / T+1 可卖量）
		if err := l.applyPosition(ctx, tx, t, req, tc); err != nil {
			return err
		}
		result.Fill = f.FillView()
		return nil
	})
	if err != nil {
		return FillResult{}, err // 账本失败上抛，禁止降级继续（§11.1）
	}
	// 成交事实已在指令单行上（reported_* 与 fill_* 列），此处只留一行日志，不再另入库。
	observability.S().Infow("成交回执已登记",
		"ticket_id", t.ID, "date", t.TradeDate, "ts_code", t.TsCode,
		"direction", string(t.Direction), "qty", int64(req.Qty), "price", int64(req.Price),
		"total_cost", int64(tc.TotalCost), "reported_by", actor)
	return result, nil
}

// applyPosition 事务内更新持仓：买入加权成本 + T+1 占用；卖出减仓。
func (l *Ledger) applyPosition(ctx context.Context, tx *sqlx.Tx, t model.OrderTicket, req FillRequest, tc market.TradeCost) error {
	var pos model.Position
	err := tx.GetContext(ctx, &pos,
		"SELECT ts_code, total_qty, today_bought, cost_price, high_price, COALESCE(first_open_date,'') AS first_open_date FROM position WHERE ts_code=?",
		t.TsCode)
	if err != nil || pos.TotalQty <= 0 {
		pos = model.Position{TsCode: t.TsCode} // 新建仓（读不到行或残留的清仓行）
	}
	switch t.Direction {
	case model.DirBuy:
		oldCost := pos.CostPrice.Mul(pos.TotalQty)
		newTotal := pos.TotalQty.Add(req.Qty)
		pos.CostPrice = oldCost.Add(tc.TotalCost).DivQty(newTotal) // (旧成本×旧量 + 成交额 + 费用) / 新量
		pos.TotalQty = newTotal
		pos.TodayBought = pos.TodayBought.Add(req.Qty) // T+1：今日买入不可卖
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
		// 只减总量：可卖量恒等于 total_qty − today_bought，卖出天然先消耗可卖部分。
		pos.TotalQty = pos.TotalQty.Sub(req.Qty)
	}
	if err := store.UpsertPositionTx(ctx, tx, pos); err != nil {
		return fmt.Errorf("更新持仓 %s 失败: %w", pos.TsCode, err)
	}
	return nil
}

// SettleT1 T+1 结转（每日 09:00）：把"昨天买的"从 today_bought 里清掉，可卖量随之变成全部持仓。
// 只写这一列：available 不落库，结转就是"昨天买的今天不算是今天买的了"。
//
// 判据是成交日而不是"有没有 today_bought"：当天补跑一次 morning_plan，
// 或按历史日期补跑，都会把当天刚买的股票解锁成可卖 —— 那是直接绕过 T+1。
func (l *Ledger) SettleT1(ctx context.Context, date string) (int, error) {
	positions, err := l.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return 0, err
	}
	lastBuy, err := l.st.TradeRepo().LastBuyDates(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, pos := range positions {
		if pos.TodayBought <= 0 {
			continue
		}
		if d := lastBuy[pos.TsCode]; d != "" && d >= date {
			continue // 这批就是结转日当天（或之后补记）买的，仍然不可卖
		}
		const q = `UPDATE position SET today_bought=0 WHERE ts_code=?`
		if _, err := l.st.WriteDB().ExecContext(ctx, q, pos.TsCode); err != nil {
			observability.S().Errorw("T+1 结转失败", "ts_code", pos.TsCode, "date", date, "err", err.Error())
			return n, fmt.Errorf("T+1 结转 %s 失败: %w", pos.TsCode, err)
		}
		observability.S().Infow("T+1 结转", "ts_code", pos.TsCode, "date", date,
			"settled_qty", int64(pos.TodayBought))
		n++
	}
	return n, nil
}

// 现金锚点两个 config 键：组合同步（券商口径校准）写入，Cash() 读取。
// 存 config_kv 而不是建新表 —— 它就是一个事实："某天券商说我还剩这么多现金"。
const (
	keyCashAnchor     = "account.cash_anchor"      // 锚点时刻的可用现金（分）
	keyCashAnchorDate = "account.cash_anchor_date" // 锚点所属交易日 YYYYMMDD
)

// Cash 推算可用现金，两种口径按有没有锚点自动切换：
//   - 有锚点（做过组合同步）：锚点现金 + 锚点之后成交的净变动。校准进来的持仓没有成交单支撑，
//     它的成本已经扣在锚点现金里，不能再从本金里减一遍；
//   - 无锚点（纯成交历史驱动，正常跑起来的账户）：本金 − Σ买入总成本 + Σ卖出净到账。
//
// 不做截断，负值即真实透支信号（正常情况下风控的现金核算会先于透支拦截）。
func (l *Ledger) Cash(ctx context.Context) (model.Fen, error) {
	raw, err := config.NewRepo(l.st).RawAll(ctx)
	if err != nil {
		return 0, fmt.Errorf("读取现金锚点失败: %w", err)
	}
	anchor, anchorDate, err := cashAnchor(raw)
	if err != nil {
		return 0, err
	}
	buyCost, sellProceeds, err := l.st.TradeRepo().FillTotals(ctx, anchorDate)
	if err != nil {
		return 0, err
	}
	if anchorDate != "" {
		return anchor - buyCost + sellProceeds, nil
	}
	return l.initialCapital - buyCost + sellProceeds, nil
}

// cashAnchor 读现金锚点。没有锚点日期＝从未做过组合同步，返回 ("") 让调用方走纯成交口径；
// 但日期在位而金额解析不了＝这个键被写坏了，必须报错——静默换口径等于现金口径被悄悄改掉。
func cashAnchor(raw map[string]string) (model.Fen, string, error) {
	date := strings.TrimSpace(raw[keyCashAnchorDate])
	if date == "" {
		return 0, "", nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw[keyCashAnchor]), 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%s=%q 不是整数（锚点日期 %s 已在位，不能当没有锚点）: %w",
			keyCashAnchor, raw[keyCashAnchor], date, err)
	}
	return model.Fen(n), date, nil
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

// PortfolioSync 一次组合同步的全部输入（口径以券商为准）。
type PortfolioSync struct {
	Date    string    // 锚点所属交易日 YYYYMMDD
	Capital model.Fen // 本金 = 期初总资产（含持仓成本）；write-once
	Cash    model.Fen // 券商口径的可用现金：必填，缺了持仓成本会被双算
	Items   []PortfolioInput
	Actor   string
}

// SyncPortfolio 以券商实际持仓校准账本：写持仓 + 写现金锚点 + 本金 write-once。
//
//   - Cash 必须 >0：校准进来的持仓没有成交单支撑，只有同时给出当时的可用现金，
//     现金推算才不会把这笔持仓成本再算成可用资金（原缺陷：同步持仓后总资产虚增一份成本）；
//   - 本金已非零 → 拒绝覆盖，返回 rejected=true（验收 #14），持仓与现金照常同步；
//   - Items 为空 = 只校准资金口径（本金 + 现金锚点），不动任何持仓行。
//
// 返回同步的持仓行数与"本金是否被拒绝覆盖"。
func (l *Ledger) SyncPortfolio(ctx context.Context, in PortfolioSync) (int, bool, error) {
	if in.Cash <= 0 {
		return 0, false, fmt.Errorf("组合同步失败: 必须给出券商口径的可用现金，" +
			"否则校准进来的持仓成本会被双算成可用资金")
	}
	actor := in.Actor
	if actor == "" {
		actor = "manual"
	}
	rejected, err := l.applyCapital(ctx, in.Date, in.Capital, actor)
	if err != nil {
		return 0, rejected, err
	}
	if err := l.applyCashAnchor(ctx, in.Date, in.Cash, actor); err != nil {
		return 0, rejected, err
	}
	n := 0
	for _, it := range in.Items {
		if it.TsCode == "" {
			return n, rejected, fmt.Errorf("组合同步失败: 第 %d 项缺少 ts_code", n+1)
		}
		pos := model.Position{
			TsCode:      it.TsCode,
			TotalQty:    it.TotalQty,
			TodayBought: it.TodayBought,
			CostPrice:   it.CostPrice,
			HighPrice:   it.HighPrice,
		}
		// 库里只有 total 与 today 两个量：券商只给了可卖量时按差额反推今日买入量，
		// 两个都给时以今日买入量为准（可卖量此后一律由 total − today 现算）。
		if pos.TodayBought <= 0 && it.AvailableQty > 0 && it.TotalQty > it.AvailableQty {
			pos.TodayBought = it.TotalQty.Sub(it.AvailableQty)
		}
		if pos.HighPrice <= 0 {
			pos.HighPrice = it.CostPrice // 校准进来的票没有历史高点，用成本起算回撤基准
		}
		if err := l.st.TradeRepo().UpsertPosition(ctx, pos); err != nil {
			return n, rejected, err
		}
		n++
	}
	observability.S().Infow("组合同步完成", "date", in.Date, "actor", actor,
		"positions", n, "cash_anchor_yuan", in.Cash.Float(),
		"capital_rejected", rejected)
	return n, rejected, nil
}

// applyCapital 本金 write-once（config account.initial_capital）。
//
// 量纲是"元"：app.InitialCapitalOf 按元读这个键，原先把分直接写进去会让本金放大 100 倍。
// 返回"是否因已配置而拒绝覆盖"。
func (l *Ledger) applyCapital(ctx context.Context, date string, capital model.Fen, actor string) (bool, error) {
	repo := config.NewRepo(l.st)
	raw, err := repo.RawAll(ctx)
	if err != nil {
		return false, fmt.Errorf("读取配置失败: %w", err)
	}
	yuan := strconv.FormatInt(int64(capital)/100, 10)
	existing, set := raw["account.initial_capital"]
	if set && existing != "" && existing != "0" {
		if existing == yuan {
			return false, nil
		}
		// 拒绝覆盖已随返回值 rejected 显式告知调用方（MCP 响应字段 capital_rejected），
		// 旧值原样留在 config_kv，不再另入轨迹表。
		observability.S().Warnw("本金 write-once 拒绝覆盖",
			"date", date, "actor", actor,
			"existing_yuan", existing, "requested_yuan", yuan,
			"reason", "initial_capital 为 write-once 配置，如需修正请走人工复核流程")
		return true, nil
	}
	if capital <= 0 {
		return false, nil // 没给本金就不动这个键（只首次写入）
	}
	if err := repo.Set(ctx, "account.initial_capital", yuan); err != nil {
		return false, fmt.Errorf("写入初始本金失败: %w", err)
	}
	l.initialCapital = capital
	return false, nil
}

// applyCashAnchor 写现金锚点（分）与它所属的交易日：锚点之前的成交视为已含在这笔现金里。
func (l *Ledger) applyCashAnchor(ctx context.Context, date string, cash model.Fen, actor string) error {
	repo := config.NewRepo(l.st)
	if err := repo.Set(ctx, keyCashAnchor, strconv.FormatInt(int64(cash), 10)); err != nil {
		return fmt.Errorf("写入现金锚点失败: %w", err)
	}
	if err := repo.Set(ctx, keyCashAnchorDate, date); err != nil {
		return fmt.Errorf("写入现金锚点日期失败: %w", err)
	}
	return nil
}
