package ticket

import (
	"context"
	"database/sql"
	"errors"
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
	// 价格量纲防呆（agent_issues #12-A）：把 6.39 元回报成 639（分当元用是反的，
	// 元当分传进来才是 100 倍放大），持仓成本与止损线会一起写坏，之后每轮扫描都判
	// "已跌破止损"并误发 M3 紧急邮件。放在幂等判定之后：已登记过的回执仍走原路返回。
	if err := req.Price.CheckTradePriceRange("成交价"); err != nil {
		return FillResult{}, fmt.Errorf("回执失败: %w: %v", ErrPriceSanity, err)
	}
	if ref, ok := l.fillPriceRef(ctx, t); ok {
		if err := req.Price.CheckPriceAgainstRef(ref, "成交价"); err != nil {
			return FillResult{}, fmt.Errorf("回执失败: %w: %v", ErrPriceSanity, err)
		}
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
		// 读不到行，或行还在但上一轮已清仓：按新开仓处理，建仓日取本笔成交日。
		// 清仓行留着旧建仓日不碍事——重开时这里会显式写新日期，覆盖是有意的
		// （旧日期属于上一轮仓，沿用它会低估持有期保护）。
		pos = model.Position{TsCode: t.TsCode}
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
	// FirstOpenDate 建仓日 YYYYMMDD，可选。券商 App 的持仓页没有这一列，
	// 平时留空即可（留空＝不改动既有建仓日，见 syncPositionOpenDate）；
	// 需要人工纠错或首次接入就知道真实建仓日时才显式给出。
	FirstOpenDate string
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
//   - Items 为空 = 只校准资金口径（本金 + 现金锚点），不动任何持仓行；
//   - 建仓日不因同步而丢失：见 syncPositionOpenDate；
//   - 价格入参过不了防呆就整次拒绝（本金/锚点/持仓都不动）：见 checkPortfolioPrices。
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
	// 建仓日与价格量纲都先整体校验，再动本金/现金锚点（两道防呆各见其函数注释）：
	// 格式错的日期比没有日期更坏（持有期会被算成
	// 天文数字，最小持有期保护反向失效），不能让它落成"半只持仓已同步、半只被拒绝"。
	if err := l.checkPortfolioPrices(ctx, in.Date, in.Items); err != nil {
		return 0, false, err
	}
	for _, it := range in.Items {
		if _, err := normalizeOpenDate(it.FirstOpenDate); err != nil {
			return 0, false, fmt.Errorf("组合同步失败: %w", err)
		}
	}
	rejected, err := l.applyCapital(ctx, in.Date, in.Capital, actor)
	if err != nil {
		return 0, rejected, err
	}
	if err := l.applyCashAnchor(ctx, in.Date, in.Cash, actor); err != nil {
		return 0, rejected, err
	}
	// 建仓日的判定基准：库里现有的那一行。同步一次读一遍，不在循环里逐只查。
	// 只取 ts_code → 行；清仓行（total_qty=0）也在，用来区分"重开仓"和"继续持有"。
	prev, err := l.currentPositions(ctx)
	if err != nil {
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
		// 券商口径没有建仓日，这一列必须由账本自己定：留空覆盖会把持有期保护的
		// 依据抹掉（agent_issues #12-B），所以按"继续持有则原样保留、新开仓才盖章"处理。
		openDate, recovered := syncPositionOpenDate(in.Date, it, prev[it.TsCode])
		pos.FirstOpenDate = openDate
		if recovered {
			// 建仓日原本缺失、由同步日补齐：这条必须响，因为它是"持有期保护刚刚被恢复"
			// 的信号，也是上一轮建仓日被谁擦掉的线索。
			observability.S().Warnw("组合同步补齐缺失的建仓日", "ts_code", it.TsCode,
				"sync_date", in.Date, "first_open_date", openDate,
				"detail", "该持仓原先没有建仓日，按同步日起算持有期（宁可低估，也不放行排名淘汰）")
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

// currentPositions 读一遍现有持仓，按代码索引。清仓行（total_qty=0）保留在结果里：
// 判断"这次同步是继续持有还是清仓后重开"必须看得见那行 0 股记录。
func (l *Ledger) currentPositions(ctx context.Context) (map[string]model.Position, error) {
	rows, err := l.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.Position, len(rows))
	for _, p := range rows {
		out[p.TsCode] = p
	}
	return out, nil
}

// syncPositionOpenDate 定组合同步要写入的建仓日，返回（建仓日，是否为补齐的缺失值）。
//
// 建仓日是持有期保护（signal.rank_out_min_hold_days）的唯一依据：擦掉它，系统就把
// "刚建仓"算成"持有期未知"，每天生成一张淘汰刚买的票的错误卖出单（agent_issues #12-B）。
// 券商持仓页没有这一列，所以同步时它只能由账本自己定，规则按"能保住就保住"排：
//
//   - 显式给了 → 以它为准（人工纠错与首次接入的唯一入口）；
//   - 券商说这只不持仓了（total_qty=0）→ 原样保留既有值，别把上一轮的日期猜成新日期；
//   - 继续持有且已有建仓日 → 原样保留，同步不改变建仓日；
//   - 继续持有但建仓日已丢失 → 按同步日补齐：把持有期估短（保护更强）远好过估成未知
//     （没有保护）；价格型退出（止损/止盈/移动止盈）不看这一列，不受影响；
//   - 新开仓或清仓后重开 → 建仓就是同步日，上一轮仓的旧日期随那一轮一起作废。
func syncPositionOpenDate(syncDate string, it PortfolioInput, prev model.Position) (string, bool) {
	if v := strings.TrimSpace(it.FirstOpenDate); v != "" {
		return v, false
	}
	if it.TotalQty <= 0 {
		return prev.FirstOpenDate, false
	}
	if prev.TotalQty > 0 {
		if prev.FirstOpenDate != "" {
			return prev.FirstOpenDate, false
		}
		return syncDate, true
	}
	return syncDate, false
}

// normalizeOpenDate 校验建仓日格式（YYYYMMDD 八位数字），返回规范化后的值。
//
// 只在这一处校验，因为持有期判定是字符串比较（signal.tradingDaysHeld）：
// "2026-10-12" 这类带分隔符的日期比任何 YYYYMMDD 都小，会被算成"持有几百天"，
// 于是最小持有期保护反向失效——那是一条比缺日期更坏的错数据。
func normalizeOpenDate(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if len(v) != 8 {
		return "", fmt.Errorf("建仓日 %q 不是 YYYYMMDD 八位数字", v)
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("建仓日 %q 含非数字字符", v)
		}
	}
	return v, nil
}

// priceReference 取某只票在 date（含）之前最近一根日线的未复权收盘价，作为量纲参照。
//
// 拿不到就返回 0（新股/停牌已久/该票尚未同步日线，或读库失败）：相对校验跳过，
// 绝对区间仍然生效。防呆不该把"紧急纠错通道"变成"因为参照缺失而彻底用不了"，
// 但读库失败必须留 warn——那说明日线链路本身有问题。
func (l *Ledger) priceReference(ctx context.Context, tsCode, date string) model.Fen {
	if tsCode == "" {
		return 0
	}
	b, err := l.st.ScreenRepo().LatestBarAt(ctx, tsCode, date)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			observability.S().Warnw("价格防呆缺少参照收盘价，本次只做绝对区间校验",
				"ts_code", tsCode, "date", date, "err", err.Error())
		}
		return 0
	}
	if b.RawClose > 0 {
		return b.RawClose
	}
	return b.Close
}

// fillPriceRef 成交回执的价格参照：优先用本单的参考价（系统给这笔交易的口径），
// 没有参考价时退回该票最近一根可用收盘。两者都拿不到 → ok=false，跳过相对校验。
func (l *Ledger) fillPriceRef(ctx context.Context, t model.OrderTicket) (model.Fen, bool) {
	if t.RefPrice > 0 {
		return t.RefPrice, true
	}
	ref := l.priceReference(ctx, t.TsCode, t.TradeDate)
	return ref, ref > 0
}

// checkPortfolioPrices 组合同步入参的价格防呆（agent_issues #12-A 的残留项 + 组长定的必填口径）。
//
// 三道闸，任一不过即**整次同步被拒**，持仓/本金/现金锚点一行都不动：
//   - 有持仓（total_qty>0）必须有 cost_price：0 成本把止损线算成 0，价格型退出保护静默消失；
//     已卖光的行（total_qty=0）不要求，免得逼调用方编一个没人核对的成本写进闭合行；
//   - 绝对区间（0.01~10000 元）挡数量级离谱的值：0、负数、万元级；
//   - 相对参照挡"5.07 元写成 507 元"——507 元本身是合法价格，绝对区间放它过去，
//     只有跟同一只票的最近收盘一比才露馅（差 100 倍）。这正是 10-15 实际发生的那次。
//
// 错误里带参照价与倍数，调用方照它与券商记录对一次单位就能定位。
func (l *Ledger) checkPortfolioPrices(ctx context.Context, date string, items []PortfolioInput) error {
	for _, it := range items {
		// 有持仓就必须给成本：缺了就落 0 成本，止损线被算成 0×(1−8%)=0，永不下穿——
		// 这只票从此没有价格型退出保护。它比量纲传错更隐蔽：单位错至少还有 M3 误报能暴露，
		// 0 成本是静默失效（组长 20261016 定：cost_price 必填）。
		// 条件必填而不是无条件：券商已卖光的那一行（total_qty=0）不算持仓、不算止损，
		// 硬要它带成本价只会逼调用方随手编一个没人核对过的数字写进已闭合的行。
		if it.TotalQty > 0 && it.CostPrice <= 0 {
			return fmt.Errorf("组合同步失败: %w: %s 持有 %d 股却没有 cost_price（元）；"+
				"0 成本会把止损线算成 0 而永不触发，请取券商持仓页的实际成本价重试",
				ErrCostMissing, it.TsCode, int64(it.TotalQty))
		}
		costLabel := fmt.Sprintf("cost_price(%s)", it.TsCode)
		highLabel := fmt.Sprintf("high_price(%s)", it.TsCode)
		ref := l.priceReference(ctx, it.TsCode, date)
		for _, p := range []struct {
			label string
			value model.Fen
		}{{costLabel, it.CostPrice}, {highLabel, it.HighPrice}} {
			if err := p.value.CheckTradePriceRange(p.label); err != nil {
				return fmt.Errorf("组合同步失败: %w: %v", ErrPriceSanity, err)
			}
			if err := p.value.CheckPriceAgainstRef(ref, p.label); err != nil {
				return fmt.Errorf("组合同步失败: %w: %v", ErrPriceSanity, err)
			}
		}
	}
	return nil
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
