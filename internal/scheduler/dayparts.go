package scheduler

// dayparts.go 除 16:30 流水线外的四个大方法：09:00 / 盘中 / 17:00 / 18:00。
// 每个大方法只做"按顺序调小方法 + 汇总错误"，功能体在各小方法里。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"jingzhe-trader/internal/attribution"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
)

// morningPlan 09:00：日历补齐 → T+1 结转 → 邮件「当日计划 + 持仓」。
func morningPlan(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	if err := renewCalendar(ctx, d, date); err != nil {
		return fmt.Errorf("① 交易日历补齐: %w", err)
	}
	if err := settleT1(ctx, rc, d, date); err != nil {
		return fmt.Errorf("② T+1 结转: %w", err)
	}
	if err := expireStaleTickets(ctx, rc, d); err != nil {
		return fmt.Errorf("②b 过期未执行单收口: %w", err)
	}
	items, tickets, err := todayPlanLines(ctx, d, date)
	if err != nil {
		return fmt.Errorf("③ 组装当日计划: %w", err)
	}
	return sendPlanMail(ctx, rc, d, date, items, tickets)
}

// renewCalendar 日历前向覆盖不足 30 个交易日则续拉（交易日判定 isTradeDay 依赖它）。
func renewCalendar(ctx context.Context, d Deps, date string) error {
	future, err := d.Store.MarketRepo().CountFutureTradeDays(ctx, date)
	if err != nil {
		return fmt.Errorf("读取日历覆盖失败: %w", err)
	}
	if future >= 30 {
		return nil
	}
	if err := d.Dataloader.SyncCalendar(ctx); err != nil {
		return fmt.Errorf("交易日历续拉失败: %w", err)
	}
	return nil
}

// settleT1 T+1 结转：昨日买入转为可卖。
func settleT1(ctx context.Context, rc *observability.RunCtx, d Deps, date string) error {
	rc.Declare("rows", "settled_positions", 0)
	n, err := d.Ledger.SettleT1(ctx, date)
	if err != nil {
		return err
	}
	rc.Actual("settled_positions", n)
	return nil
}

// expireStaleTickets 把过了有效期仍没人执行的单收成 expired（每天开工先做这件事）。
func expireStaleTickets(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	rc.Declare("rows", "tickets_expired", 0)
	n, err := d.Store.TradeRepo().ExpireStale(ctx, time.Now().In(market.Loc).Format(time.RFC3339))
	if err != nil {
		return err
	}
	rc.Actual("tickets_expired", n)
	observability.S().Infow("过期未执行指令单已收口", "date", rc.TradeDate(), "expired", n)
	return nil
}

// todayPlanLines 当日计划 = 未过期指令单 ∪ 持仓（含各自止损参考线）。
//
// 指令单按"活跃且未过期"取，不按生成日过滤：前一交易日 16:30 生成的单，trade_date 是
// 那一天，早上按"今天"去查会得到空表（历史缺陷）——有效期才是"该不该执行"的判据。
// 第二个返回值是同一批指令单的邮件行（供 M2 内嵌条件单速填参数块）。
func todayPlanLines(ctx context.Context, d Deps, date string) ([]string, []notify.TicketLine, error) {
	now := time.Now().In(market.Loc).Format(time.RFC3339)
	acts, err := d.Store.TradeRepo().ListActiveUnexpired(ctx, now)
	if err != nil {
		return nil, nil, fmt.Errorf("读取待买卖表失败: %w", err)
	}
	var items []string
	var tickets []notify.TicketLine
	for _, t := range acts {
		if !t.IsActive() {
			continue
		}
		items = append(items, fmt.Sprintf("待执行 #%d %s %s %d 股，参考价 %s 元，有效期至 %s",
			t.ID, t.TsCode, t.Direction.Label(), int64(t.Qty),
			fmtYuan(int64(t.RefPrice)), t.ValidUntil))
		tickets = append(tickets, notify.TicketLine{
			TsCode: t.TsCode, Name: t.Name, Direction: string(t.Direction), DirLabel: t.Direction.Label(),
			Qty: int64(t.Qty), Price: float64(t.RefPrice) / 100,
			ValidUntil: t.ValidUntil, Reason: t.Reason,
		})
	}
	posLines, err := positionLines(ctx, d, date)
	if err != nil {
		return nil, nil, err
	}
	return append(items, posLines...), tickets, nil
}

// tradeConclusion 最近一次收盘流水线给当日留下的人话结论（供计划/日报引用）。
//
// 数据来源是 job:evening_pipeline 轨迹行：它的 Detail 在"有单"时为空，
// 在"无单/降级"时写着原因（SCREEN_EMPTY 的关闸说明、ALL_REJECTED、NO_TICKET_FROM_CANDIDATES）。
// 当日 16:30 之前该行还不存在，故按 ≤date 取最近一行（通常是上一交易日的）。
func tradeConclusion(ctx context.Context, d Deps, date string) string {
	t, ok, err := d.Store.TraceRepo().LatestBySubject(ctx, model.TraceJob(JobEveningPipeline), date)
	if err != nil {
		return "（读取上一交易日结论失败：" + err.Error() + "）"
	}
	if !ok {
		return "（暂无收盘流水线记录）"
	}
	if t.Outcome == model.TraceOK && t.Detail == "" {
		return fmt.Sprintf("收盘流水线（%s）正常跑完，见下方待执行指令", t.TradeDate)
	}
	return fmt.Sprintf("收盘流水线（%s）：%s", t.TradeDate, t.Detail)
}

// positionLines 持仓逐条：成本与止损参考线（风控参数读不到就整段失败）。
func positionLines(ctx context.Context, d Deps, date string) ([]string, error) {
	pos, err := d.Store.TradeRepo().ListPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取持仓失败: %w", err)
	}
	rp, err := d.RiskParams(ctx, date)
	if err != nil {
		return nil, fmt.Errorf("读取止损参考线所需风控参数失败: %w", err)
	}
	var out []string
	for _, p := range pos {
		if p.TotalQty <= 0 {
			continue // 清仓行不是持仓
		}
		if p.CostPrice <= 0 {
			return nil, fmt.Errorf("持仓 %s 成本非法（cost=%d），算不出止损参考线",
				p.TsCode, int64(p.CostPrice))
		}
		stop := int64(float64(p.CostPrice) * (1 - rp.StopLossPct))
		out = append(out, fmt.Sprintf("持仓 %s %d 股，成本 %s 元，止损线 %s 元",
			p.TsCode, int64(p.TotalQty), fmtYuan(int64(p.CostPrice)), fmtYuan(stop)))
	}
	return out, nil
}

// sendPlanMail 发「当日计划 + 持仓」邮件（M2）。计划为空也要发——它是开盘前的确认凭据。
//
// 邮件没发出去就是任务失败：以前配置缺失只记一条降级并返回 nil，
// 于是"任务绿、零邮件"这个历史缺陷（D1）每天都发生一遍还没人看得见。
func sendPlanMail(ctx context.Context, rc *observability.RunCtx, d Deps, date string, items []string, tickets []notify.TicketLine) error {
	if len(items) == 0 {
		items = []string{"今日无待执行指令、无持仓"}
	}
	rc.Declare("mail", "plan_m2", -1)
	brief, err := accountBriefOf(d, ctx, date)
	if err != nil {
		return fmt.Errorf("账户摘要读取失败: %w", err)
	}
	subject, body := notify.RenderM2(items, tickets, brief, tradeConclusion(ctx, d, date))
	if err := d.Mail.Send(ctx, date, model.MailM2, subject, body); err != nil {
		d.raiseW(rc, "MAIL_NOT_SENT", "计划邮件发送失败", err.Error())
		return fmt.Errorf("计划邮件(M2)发送失败: %w", err)
	}
	rc.Actual("plan_m2", 1)
	return nil
}

// intradayScan 盘中每 5 分钟：扫持仓是否触发价格型卖出规则
// （止损 / 移动止盈 / 止盈），需要卖出的写待买卖表并即时发信。
//
// 去重：该股在待买卖表里已有未执行卖单时，既不重复建单也不重复发信。
// 排名淘汰与大盘恶化不在此列——它们是日频重平衡概念，见 signal.EvalPriceRules 的说明。
func intradayScan(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	holding, err := d.Store.TradeRepo().HoldingCodes(ctx)
	if err != nil {
		return fmt.Errorf("读取持仓代码失败: %w", err)
	}
	if len(holding) == 0 {
		// 空仓时这一轮没有任何可判定的对象：按 ErrSkip 退出，不写轨迹。
		// 用 Degrade 会把每 5 分钟一轮的空转写成 partial，一天 24 行盖掉
		// 真正需要看出的降级，也会让日报的降级列长期挂着"盘中扫描"。
		return fmt.Errorf("%w: 无持仓", ErrSkip)
	}
	quoted, err := fetchQuotes(ctx, rc, d, holding)
	if err != nil {
		return err
	}
	rp, err := d.RiskParams(ctx, date)
	if err != nil {
		return fmt.Errorf("读取生效风控参数失败: %w", err)
	}
	pending, err := pendingSellSet(ctx, d, date)
	if err != nil {
		return err
	}
	fresh, err := stopLossTickets(ctx, rc, d, date, holding, quoted, rp, pending)
	if err != nil {
		return err
	}
	return notifySell(ctx, rc, d, date, fresh)
}

// fetchQuotes 取持仓实时价；取不到即失败（不据此判断卖出，也不假装判断过了）。
func fetchQuotes(ctx context.Context, rc *observability.RunCtx, d Deps, codes []string) (map[string]quote.Quote, error) {
	rc.Declare("quotes", "fetched_codes", len(codes))
	qs, err := d.Quote.Fetch(ctx, codes)
	if err != nil {
		d.raiseU(rc, "QUOTE_FETCH_FAILED", "盘中取价失败，本轮卖出未判定", err.Error())
		return nil, fmt.Errorf("盘中取价失败: %w", err)
	}
	rc.Actual("fetched_codes", len(qs))
	return qs, nil
}

// pendingSellSet 待买卖表中已有未执行卖单的股票代码集合。
func pendingSellSet(ctx context.Context, d Deps, date string) (map[string]bool, error) {
	acts, err := d.Store.TradeRepo().ListActiveTickets(ctx, date)
	if err != nil {
		return nil, fmt.Errorf("读取待买卖表失败: %w", err)
	}
	set := make(map[string]bool, len(acts))
	for _, t := range acts {
		if t.Direction == model.DirSell && t.IsActive() {
			set[t.TsCode] = true
		}
	}
	return set, nil
}

// stopLossTickets 逐只持仓：先抬升期间最高价（移动止盈的回撤基准），
// 再按价格型卖出规则（止损/移动止盈/止盈）判定，触发则建卖出指令单。
// 返回本轮新增单（供发信）。
func stopLossTickets(ctx context.Context, rc *observability.RunCtx, d Deps, date string,
	holding []string, quoted map[string]quote.Quote, rp risk.RiskParams,
	pending map[string]bool) ([]notify.TicketLine, error) {

	days, err := d.Store.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取交易日列表失败: %w", err)
	}
	var out []notify.TicketLine
	for _, code := range holding {
		q, ok := quoted[code]
		if !ok {
			return nil, fmt.Errorf("持仓 %s 无报价（行情源漏返回），本轮卖出未判定", code)
		}
		if q.Price <= 0 {
			return nil, fmt.Errorf("持仓 %s 报价非法（price=%d），无法判断卖出", code, int64(q.Price))
		}
		// 先用现价抬升高点：移动止盈的回撤基准必须反映当日盘中创出的新高，
		// 否则"高点"停在买入价，回撤线永远够不着（历史缺陷）。
		if err := d.Store.TradeRepo().RaiseHighPrice(ctx, code, q.Price); err != nil {
			return nil, err
		}
		if pending[code] {
			continue // 已有未执行卖单：不重复建单也不重复发信
		}
		// 抬升之后再读持仓：移动止盈要用到刚更新的 high_price。
		pos, err := d.Store.TradeRepo().GetPosition(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("读取持仓 %s 失败: %w", code, err)
		}
		if pos.TotalQty <= 0 {
			return nil, fmt.Errorf("持仓 %s 在持仓表里数量为 0，与盘中扫描口径不一致", code)
		}
		if pos.CostPrice <= 0 {
			return nil, fmt.Errorf("持仓 %s 成本非法（cost=%d），算不出卖出线", code, int64(pos.CostPrice))
		}
		sig := signal.EvalPriceRules(date, signal.HoldingCtx{
			Pos: pos, LastClose: q.Price, LastDate: date,
		}, rp)
		if sig == nil {
			continue
		}
		name, err := d.Signal.SellName(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("卖出单 %s 取名称失败（stock_basic 未同步？）: %w", code, err)
		}
		sig.Name = name
		sig.RefPrice = q.Price
		tk, err := d.Tickets.Create(ctx, *sig, pos.Available(), days)
		if err != nil {
			return nil, fmt.Errorf("创建卖出指令单 %s 失败: %w", code, err)
		}
		out = append(out, notify.TicketLine{
			TsCode: tk.TsCode, Name: tk.Name, Direction: string(tk.Direction), DirLabel: tk.Direction.Label(),
			Qty: int64(tk.Qty), Price: float64(tk.RefPrice) / 100,
			ValidUntil: tk.ValidUntil, Reason: tk.Reason,
		})
		// 只落轨迹不发 M6：本轮卖单会由 notifySell 发 M3，双发就是同一件事两封邮件。
		d.raiseUTrace(rc, "INTRADAY_SELL:"+code+":"+date, "盘中卖出触发", sig.Reason)
	}
	return out, nil
}

// notifySell 有新卖单才发 M3（每轮最多一封，含本轮全部新单）。发不出去即任务失败。
func notifySell(ctx context.Context, rc *observability.RunCtx, d Deps, date string, fresh []notify.TicketLine) error {
	if len(fresh) == 0 {
		return nil
	}
	rc.Declare("mail", "stoploss_m3", -1)
	subject, body := notify.RenderM3(fresh, "盘中触发卖出")
	if err := d.Mail.Send(ctx, date, model.MailM3, subject, body); err != nil {
		d.raiseW(rc, "MAIL_NOT_SENT", "卖出邮件发送失败", err.Error())
		return fmt.Errorf("卖出邮件(M3)发送失败: %w", err)
	}
	rc.Actual("stoploss_m3", 1)
	return nil
}

// mailPending 17:00：下发指令单（drafted→issued）。
//
// M1 已取消（2026-10-23 组长定）：其内容与 18:00 M5 的【次日计划】+ 条件单参数重复。
// IssueAll 仍必须执行——次日盘前 M2 的条件单参数与"已下发"状态都依赖它。
func mailPending(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	issued, err := d.Tickets.IssueAll(ctx, date)
	if err != nil {
		return fmt.Errorf("指令单下发失败: %w", err)
	}
	observability.S().Infow("指令单下发完成（M1 已并入 M5，不发信）", "date", date, "issued", issued)
	return nil
}

// pendingTicketLines 待买卖表全部未执行单 → 邮件行。
func pendingTicketLines(ctx context.Context, d Deps, date string) ([]notify.TicketLine, error) {
	acts, err := d.Store.TradeRepo().ListActiveTickets(ctx, date)
	if err != nil {
		return nil, fmt.Errorf("读取待买卖表失败: %w", err)
	}
	var lines []notify.TicketLine
	for _, t := range acts {
		if !t.IsActive() {
			continue
		}
		lines = append(lines, notify.TicketLine{
			TsCode: t.TsCode, Name: t.Name, Direction: string(t.Direction), DirLabel: t.Direction.Label(),
			Qty: int64(t.Qty), Price: float64(t.RefPrice) / 100,
			ValidUntil: t.ValidUntil, Reason: t.Reason,
		})
	}
	return lines, nil
}

// dailyReport 18:00：日报（当天总结 + 持仓 + 次日计划 + 当天失败汇报），随后保留清理。
func dailyReport(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	if err := sendDailyReport(ctx, rc, d, date); err != nil {
		return fmt.Errorf("① 日报: %w", err)
	}
	return applyRetention(ctx, rc, d)
}

// sendDailyReport 组装并发送 M5：正文 = 自检区块 + 持仓 + 次日计划，任务按成功/降级/失败分列。
func sendDailyReport(ctx context.Context, rc *observability.RunCtx, d Deps, date string) error {
	rc.Declare("mail", "daily_m5", -1)
	block := BuildDailyBlock(ctx, d.Store, date, d.MinBarRows)
	extra, err := reportExtraSections(ctx, d, date)
	if err != nil {
		return err
	}
	okJobs, degJobs, failJobs, err := jobLines(ctx, d.Store, date)
	if err != nil {
		return fmt.Errorf("任务清单读取失败，日报不能装作今天没有失败: %w", err)
	}
	ast, err := d.Ledger.Assets(ctx, date)
	if err != nil {
		return fmt.Errorf("账户摘要读取失败: %w", err)
	}
	// 先落当日组合快照，再算 5/10/20 日表现：快照是历史比较的唯一数据源，不建资产曲线表。
	// 写失败只降级告警，不掐日报——日报仍有当日资产与任务状态可看。
	todaySnap, snapErr := writePortfolioSnapshot(ctx, d, date, ast)
	if snapErr != nil {
		d.raiseW(rc, "PORTFOLIO_SNAPSHOT_FAILED", "组合快照写入失败", snapErr.Error())
	}
	extra += portfolioPerformanceSection(ctx, d, date, todaySnap)
	brief := notify.Brief{CashYuan: float64(ast.Cash) / 100, TotalYuan: float64(ast.TotalAsset) / 100}
	subject, body := notify.RenderM5(date, block+extra, okJobs, degJobs, failJobs, brief, tradeConclusion(ctx, d, date))
	if err := d.Mail.Send(ctx, date, model.MailM5, subject, body); err != nil {
		// warning 而非 urgent：任务本身已 fail，调度器统一落 JOB_FAILED urgent 发 M6；
		// 这里再发一封是同一根因两条紧急告警（其余三处邮件失败也都是 warning）。
		d.raiseW(rc, "MAIL_NOT_SENT", "日报发送失败", err.Error())
		return fmt.Errorf("日报发送失败: %w", err)
	}
	rc.Actual("daily_m5", 1) // 发出去才算交付（入队即 Actual 是 [D1] 的假绿）
	return nil
}

// writePortfolioSnapshot 落当日组合快照（run_trace subject=portfolio:snapshot）。
// 指数收盘缺失时仍写资产部分（IndexCloseFen=0），后续表现计算会跳过该点。
func writePortfolioSnapshot(ctx context.Context, d Deps, date string, ast model.Assets) (model.PortfolioSnapshot, error) {
	snap := model.PortfolioSnapshot{
		TradeDate:      date,
		TotalAssetFen:  int64(ast.TotalAsset),
		CashFen:        int64(ast.Cash),
		MarketValueFen: int64(ast.MarketValue),
		PositionCount:  ast.PositionCount,
	}
	if bar, err := d.Store.ScreenRepo().LatestBarAt(ctx, store.MarketIndex, date); err != nil {
		observability.S().Warnw("组合快照缺少 000300 收盘", "date", date, "err", err.Error())
	} else {
		snap.IndexCloseFen = int64(bar.Close)
	}
	detail, err := json.Marshal(snap)
	if err != nil {
		return snap, fmt.Errorf("序列化组合快照失败: %w", err)
	}
	if err := d.Store.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: date, Subject: model.TracePortfolioSnapshot(), Outcome: model.TraceOK,
		Detail: string(detail), At: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return snap, fmt.Errorf("写入组合快照失败: %w", err)
	}
	return snap, nil
}

// portfolioPerformanceSection 组合 5/10/20 个交易日涨跌 vs 000300。
// 数据源只有 portfolio:snapshot；历史不足（缺快照或指数收盘）就跳过该档，不补造数据。
// 口径为总资产（含现金）变动，期间有出入金会失真——当前账户没有出入金记录，接受此近似。
func portfolioPerformanceSection(ctx context.Context, d Deps, date string, today model.PortfolioSnapshot) string {
	if today.TotalAssetFen <= 0 || today.IndexCloseFen <= 0 {
		return ""
	}
	dates, err := d.Store.MarketRepo().TradeDateList(ctx)
	if err != nil {
		observability.S().Warnw("组合表现读取交易日失败", "date", date, "err", err.Error())
		return ""
	}
	pos := sort.SearchStrings(dates, date)
	if pos >= len(dates) || dates[pos] != date {
		observability.S().Warnw("组合表现：当日不在交易日列表", "date", date)
		return ""
	}
	var lines []string
	for _, h := range []int{5, 10, 20} {
		if pos-h < 0 {
			continue
		}
		oldDate := dates[pos-h]
		old, ok := readPortfolioSnapshot(ctx, d, oldDate)
		if !ok || old.TotalAssetFen <= 0 || old.IndexCloseFen <= 0 {
			continue
		}
		portRet := (float64(today.TotalAssetFen)/float64(old.TotalAssetFen) - 1) * 100
		indexRet := (float64(today.IndexCloseFen)/float64(old.IndexCloseFen) - 1) * 100
		lines = append(lines, fmt.Sprintf("%d日：组合 %+.2f%% vs 000300 %+.2f%%（超额 %+.2f pct）",
			h, portRet, indexRet, portRet-indexRet))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n【组合表现 vs 000300】\n  " + strings.Join(lines, "\n  ") + "\n"
}

// readPortfolioSnapshot 读指定交易日的组合快照；没有或解析失败返回 false。
func readPortfolioSnapshot(ctx context.Context, d Deps, date string) (model.PortfolioSnapshot, bool) {
	traces, err := d.Store.TraceRepo().List(ctx, date)
	if err != nil {
		observability.S().Warnw("读取组合快照失败", "date", date, "err", err.Error())
		return model.PortfolioSnapshot{}, false
	}
	for _, tr := range traces {
		if tr.Subject != model.TracePortfolioSnapshot() {
			continue
		}
		var snap model.PortfolioSnapshot
		if err := json.Unmarshal([]byte(tr.Detail), &snap); err != nil {
			observability.S().Warnw("组合快照解析失败", "date", date, "err", err.Error())
			return model.PortfolioSnapshot{}, false
		}
		return snap, true
	}
	return model.PortfolioSnapshot{}, false
}

// reportExtraSections 日报追加两段：当前持仓、次日计划（= 待买卖表未执行单）。
func reportExtraSections(ctx context.Context, d Deps, date string) (string, error) {
	pos, err := d.Store.TradeRepo().ListPositions(ctx)
	if err != nil {
		return "", fmt.Errorf("读取持仓失败: %w", err)
	}
	var b strings.Builder
	b.WriteString("\n【当前持仓】\n")
	if len(pos) == 0 {
		b.WriteString("  空仓\n")
	}
	for _, p := range pos {
		if p.TotalQty <= 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("  %s %d 股 成本 %s 元\n", p.TsCode, int64(p.TotalQty), fmtYuan(int64(p.CostPrice))))
	}
	lines, err := pendingTicketLines(ctx, d, date)
	if err != nil {
		return "", err
	}
	b.WriteString("\n【次日计划】\n")
	if len(lines) == 0 {
		b.WriteString("  无待执行指令\n")
	}
	for _, l := range lines {
		b.WriteString(fmt.Sprintf("  %s %s %d 股 %.2f 元：%s\n",
			l.TsCode, l.DirLabel, l.Qty, l.Price, l.Reason))
		for _, ln := range strings.Split(notify.CondOrderBlock(l), "\n") {
			b.WriteString("  " + ln + "\n")
		}
	}
	// 候选归因：只读 run_trace/daily_bar，失败不影响日报本身（诊断缺一段不能把日报掐掉）。
	b.WriteString("\n【候选归因】\n")
	b.WriteString("  " + candidateIndustryLine(ctx, d.Store, date) + "\n")
	for _, h := range []int{5, 10} {
		line, aerr := attribution.SummaryLine(ctx, d.Store, date, 20, h)
		if aerr != nil {
			observability.S().Warnw("日报归因不可用", "date", date, "horizon", h, "err", aerr.Error())
			b.WriteString(fmt.Sprintf("  %d日：归因不可用（见日志）\n", h))
			continue
		}
		b.WriteString("  " + line + "\n")
	}
	return b.String(), nil
}

// candidateIndustryLine 日报候选段的行业分布：读当日 screen:topn 快照的前 20 只，
// 各行业只数降序。只读已落库的快照（不做第二次选股），缺快照时明说"无"而不是留空。
func candidateIndustryLine(ctx context.Context, st *store.Store, date string) string {
	snaps, err := st.TraceRepo().ListScreenTopN(ctx, date, date)
	if err != nil {
		observability.S().Warnw("日报候选行业分布读取失败", "date", date, "err", err.Error())
		return "候选行业分布：读取失败（见日志）"
	}
	snap, ok := snaps[date]
	if !ok {
		return "候选行业分布：当日无候选快照"
	}
	n := len(snap.Candidates)
	if n > 20 {
		n = 20
	}
	if n == 0 {
		return "候选行业分布：当日 0 只"
	}
	counts := make(map[string]int, n)
	for _, c := range snap.Candidates[:n] {
		ind := strings.TrimSpace(c.Industry)
		if c.Kind == model.KindETF {
			ind = "ETF"
		}
		if ind == "" {
			ind = "未知"
		}
		counts[ind]++
	}
	type kv struct {
		name string
		n    int
	}
	list := make([]kv, 0, len(counts))
	for k, v := range counts {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].n != list[j].n {
			return list[i].n > list[j].n
		}
		return list[i].name < list[j].name
	})
	parts := make([]string, 0, len(list))
	for _, it := range list {
		parts = append(parts, fmt.Sprintf("%s%d", it.name, it.n))
	}
	return fmt.Sprintf("候选行业分布（前 %d 只）：%s", n, strings.Join(parts, "、"))
}

// applyRetention 保留清理 + WAL checkpoint（放在日报之后：清理失败不影响当日通知）。
func applyRetention(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	now := time.Now()
	if d.RetentionNow != nil {
		now = d.RetentionNow()
	}
	deletedMap, err := store.ApplyRetention(ctx, d.Store, now, d.Retention)
	if err != nil {
		return fmt.Errorf("保留清理失败: %w", err)
	}
	tables := make([]string, 0, len(deletedMap))
	for tbl := range deletedMap {
		tables = append(tables, tbl)
	}
	sort.Strings(tables)
	total := 0
	for _, tbl := range tables {
		total += deletedMap[tbl]
		observability.S().Infow("保留清理", "table", tbl, "deleted_rows", deletedMap[tbl])
	}
	rc.Declare("rows", "deleted_rows", 0)
	rc.Actual("deleted_rows", total)
	if err := store.IncrementalVacuum(ctx, d.Store, 0); err != nil {
		return fmt.Errorf("增量 vacuum 失败: %w", err)
	}
	if err := store.WALCheckpoint(ctx, d.Store); err != nil {
		return fmt.Errorf("WAL checkpoint 失败: %w", err)
	}
	return nil
}
