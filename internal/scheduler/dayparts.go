package scheduler

// dayparts.go 除 16:30 流水线外的四个大方法：09:00 / 盘中 / 17:00 / 18:00。
// 每个大方法只做"按顺序调小方法 + 汇总错误"，功能体在各小方法里。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/risk"
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
	items, err := todayPlanLines(ctx, d, date)
	if err != nil {
		return fmt.Errorf("③ 组装当日计划: %w", err)
	}
	return sendPlanMail(ctx, rc, d, date, items)
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

// todayPlanLines 当日计划 = 未执行指令单 ∪ 持仓（含各自止损参考线）。
func todayPlanLines(ctx context.Context, d Deps, date string) ([]string, error) {
	acts, err := d.Store.TradeRepo().ListActiveTickets(ctx, date)
	if err != nil {
		return nil, fmt.Errorf("读取待买卖表失败: %w", err)
	}
	var items []string
	for _, t := range acts {
		if !t.IsActive() {
			continue
		}
		items = append(items, fmt.Sprintf("待执行 #%d %s %s %d 股，参考价 %s 元，有效期至 %s",
			t.ID, t.TsCode, t.Direction.Label(), int64(t.Qty),
			fmtYuan(int64(t.RefPrice)), t.ValidUntil))
	}
	posLines, err := positionLines(ctx, d, date)
	if err != nil {
		return nil, err
	}
	return append(items, posLines...), nil
}

// positionLines 持仓逐条：成本与止损参考线（风控参数读不到就整段失败）。
func positionLines(ctx context.Context, d Deps, date string) ([]string, error) {
	pos, err := d.Store.TradeRepo().ListPositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取持仓失败: %w", err)
	}
	rp, _, err := d.RiskParams(ctx, date)
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
func sendPlanMail(ctx context.Context, rc *observability.RunCtx, d Deps, date string, items []string) error {
	if len(items) == 0 {
		items = []string{"今日无待执行指令、无持仓"}
	}
	rc.Declare("mail", "plan_m2", -1)
	brief, err := goalBriefOf(d, ctx, date)
	if err != nil {
		return fmt.Errorf("目标概要读取失败: %w", err)
	}
	subject, body := notify.RenderM2(items, brief)
	if err := d.Mail.Send(ctx, date, model.MailM2, subject, body); err != nil {
		d.raiseW(rc, "MAIL_NOT_SENT", "计划邮件发送失败", err.Error())
		return fmt.Errorf("计划邮件(M2)发送失败: %w", err)
	}
	rc.Actual("plan_m2", 1)
	return nil
}

// intradayScan 盘中每 5 分钟：扫持仓是否跌破止损线，需要卖出的写待买卖表并即时发信。
//
// 去重：该股在待买卖表里已有未执行卖单时，既不重复建单也不重复发信。
func intradayScan(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	holding, err := d.Store.TradeRepo().HoldingCodes(ctx)
	if err != nil {
		return fmt.Errorf("读取持仓代码失败: %w", err)
	}
	if len(holding) == 0 {
		rc.Degrade("NO_HOLDINGS", "无持仓，盘中扫描空转")
		return nil
	}
	quoted, err := fetchQuotes(ctx, rc, d, holding)
	if err != nil {
		return err
	}
	rp, gear, err := d.RiskParams(ctx, date)
	if err != nil {
		return fmt.Errorf("读取生效风控参数失败: %w", err)
	}
	pending, err := pendingSellSet(ctx, d, date)
	if err != nil {
		return err
	}
	fresh, err := stopLossTickets(ctx, rc, d, date, holding, quoted, rp, gear, pending)
	if err != nil {
		return err
	}
	return notifyStopLoss(ctx, rc, d, date, fresh)
}

// fetchQuotes 取持仓实时价；取不到即失败（不据此判断止损，也不假装判断过了）。
func fetchQuotes(ctx context.Context, rc *observability.RunCtx, d Deps, codes []string) (map[string]quote.Quote, error) {
	rc.Declare("quotes", "fetched_codes", len(codes))
	qs, err := d.Quote.Fetch(ctx, codes)
	if err != nil {
		d.raiseU(rc, "QUOTE_FETCH_FAILED", "盘中取价失败，本轮止损未判定", err.Error())
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

// stopLossTickets 跌破止损线的持仓 → 建卖出指令单，返回本轮新增单（供发信）。
func stopLossTickets(ctx context.Context, rc *observability.RunCtx, d Deps, date string,
	holding []string, quoted map[string]quote.Quote, rp risk.RiskParams, gear model.Gear,
	pending map[string]bool) ([]notify.TicketLine, error) {

	days, err := d.Store.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取交易日列表失败: %w", err)
	}
	var out []notify.TicketLine
	for _, code := range holding {
		if pending[code] {
			continue // 已有未执行卖单：不重复建单也不重复发信
		}
		q, ok := quoted[code]
		if !ok {
			return nil, fmt.Errorf("持仓 %s 无报价（行情源漏返回），本轮止损未判定", code)
		}
		if q.Price <= 0 {
			return nil, fmt.Errorf("持仓 %s 报价非法（price=%d），无法判断止损", code, int64(q.Price))
		}
		pos, err := d.Store.TradeRepo().GetPosition(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("读取持仓 %s 失败: %w", code, err)
		}
		if pos.TotalQty <= 0 {
			return nil, fmt.Errorf("持仓 %s 在持仓表里数量为 0，与盘中扫描口径不一致", code)
		}
		if pos.CostPrice <= 0 {
			return nil, fmt.Errorf("持仓 %s 成本非法（cost=%d），算不出止损线", code, int64(pos.CostPrice))
		}
		stop := model.Fen(float64(pos.CostPrice) * (1 - rp.StopLossPct))
		if q.Price > stop {
			continue
		}
		name, err := d.Store.ScreenRepo().StockName(ctx, code)
		if err != nil {
			return nil, fmt.Errorf("止损单 %s 取名称失败（stock_basic 未同步？）: %w", code, err)
		}
		sig := model.Signal{
			TradeDate: date, TsCode: code, Name: name, Direction: model.DirSell, Rule: "intraday_stop",
			Confidence: 1.0, RefPrice: q.Price,
			Reason: fmt.Sprintf("盘中现价 %s 元 ≤ 止损线 %s 元", fmtYuan(int64(q.Price)), fmtYuan(int64(stop))),
		}
		tk, err := d.Tickets.Create(ctx, sig, pos.Available(), gear, days)
		if err != nil {
			return nil, fmt.Errorf("创建止损指令单 %s 失败: %w", code, err)
		}
		out = append(out, notify.TicketLine{
			TsCode: tk.TsCode, Name: tk.Name, Direction: string(tk.Direction), DirLabel: tk.Direction.Label(),
			Qty: int64(tk.Qty), Price: float64(tk.RefPrice) / 100,
			ValidUntil: tk.ValidUntil, Reason: tk.Reason,
		})
		d.raiseU(rc, "INTRADAY_STOP:"+code+":"+date, "盘中止损触发", sig.Reason)
	}
	return out, nil
}

// notifyStopLoss 有新卖单才发 M3（每轮最多一封，含本轮全部新单）。发不出去即任务失败。
func notifyStopLoss(ctx context.Context, rc *observability.RunCtx, d Deps, date string, fresh []notify.TicketLine) error {
	if len(fresh) == 0 {
		return nil
	}
	rc.Declare("mail", "stoploss_m3", -1)
	subject, body := notify.RenderM3(fresh, "盘中触发止损")
	if err := d.Mail.Send(ctx, date, model.MailM3, subject, body); err != nil {
		d.raiseW(rc, "MAIL_NOT_SENT", "止损邮件发送失败", err.Error())
		return fmt.Errorf("止损邮件(M3)发送失败: %w", err)
	}
	rc.Actual("stoploss_m3", 1)
	return nil
}

// mailPending 17:00：下发指令单，读待买卖表发邮件；表为空则不发。
func mailPending(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	issued, err := d.Tickets.IssueAll(ctx, date)
	if err != nil {
		return fmt.Errorf("指令单下发失败: %w", err)
	}
	lines, err := pendingTicketLines(ctx, d, date)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		rc.Degrade("NO_PENDING_TICKET", fmt.Sprintf("待买卖表为空（本轮下发 %d 张），不发信", issued))
		return nil
	}
	rc.Declare("mail", "pending_m1", -1)
	brief, err := goalBriefOf(d, ctx, date)
	if err != nil {
		return fmt.Errorf("目标概要读取失败: %w", err)
	}
	subject, body := notify.RenderM1(lines, brief, "")
	if err := d.Mail.Send(ctx, date, model.MailM1, subject, body); err != nil {
		d.raiseW(rc, "MAIL_NOT_SENT", "待买卖邮件发送失败", err.Error())
		return fmt.Errorf("待买卖邮件(M1)发送失败: %w", err)
	}
	rc.Actual("pending_m1", 1)
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
	brief, err := goalBriefOf(d, ctx, date)
	if err != nil {
		return fmt.Errorf("目标概要读取失败: %w", err)
	}
	subject, body := notify.RenderM5(date, block+extra, okJobs, degJobs, failJobs, brief)
	if err := d.Mail.Send(ctx, date, model.MailM5, subject, body); err != nil {
		// warning 而非 urgent：任务本身已 fail，调度器统一落 JOB_FAILED urgent 发 M6；
		// 这里再发一封是同一根因两条紧急告警（其余三处邮件失败也都是 warning）。
		d.raiseW(rc, "MAIL_NOT_SENT", "日报发送失败", err.Error())
		return fmt.Errorf("日报发送失败: %w", err)
	}
	rc.Actual("daily_m5", 1) // 发出去才算交付（入队即 Actual 是 [D1] 的假绿）
	return nil
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
	}
	return b.String(), nil
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
