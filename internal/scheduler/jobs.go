package scheduler

// jobs.go §5.2 交易日时间线的 14 个 JobSpec 装配。
//
// 顺序即触发优先顺序（同一时刻多个任务按此顺序执行）：
//  1. daily_startup_check 07:00        晨检：日历续拉 / 昨日未完成补跑判定 / 库目录同步标记 / 邮件配置自检
//  2. premarket_check     08:30        盘前提醒（条件触发 M2）
//  3. t1_settle           09:25        T+1 结转
//  4. intraday_watch      09:30-11:30/13:00-15:00 每 5 分钟  盘中监控与止损
//  5. expire_overdue      15:00        收盘后过期扫描（issued → expired）
//  6. data_sync           15:05        数据更新（4 重试窗口中的第 1 次）
//  7. snapshot_goal       15:20        账户快照 + 季度档位评估
//  8. screener            15:30        全市场选股
//  9. llm_review          15:40        LLM 终审配置自检（终审在 15:50 信号任务内联执行）
// 10. data_retry_2        15:50        数据更新重试窗口 2
// 11. signal              15:50        信号生成（前置数据门禁）
// 12. ticket_mail         16:10        次日指令邮件 M1
// 13. data_retry_3        16:30        数据更新重试窗口 3
// 14. cleanup             16:40        保留清理 + WAL checkpoint
// 15. daily_report        20:00        每日报告 M5（心跳必发，含静默失败自检区块）
//
// 注：data_sync 三个重试窗口按独立 JobSpec 落地（data_sync/data_retry_2/data_retry_3），
// 以保证任一窗口成功后后续窗口自动跳过（HasJobSucceeded 判定），与"4 个重试时间点"对应。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// Deps 任务依赖集合（由 app.Wire 装配）。
type Deps struct {
	Store        *store.Store
	Dataloader   *dataloader.Dataloader
	Freshness    *dataloader.FreshnessGate
	Quote        quote.Source
	Screener     *screener.Screener
	Signal       *signal.Service
	Confirmer    signal.BuyConfirmer
	Ledger       *ticket.Ledger
	Tickets      *ticket.Service
	Goal         GoalService
	Alerts       AlertService
	Mail         *notify.Mailer
	RiskParams   func(ctx context.Context, date string) (risk.RiskParams, model.Gear, error)
	FilterCfg    screener.FilterConfig
	MinBarRows   int
	RetentionNow func() time.Time
}

// GoalService 目标服务最小接口（避免 scheduler → goal 的编译耦合由 app 注入）。
type GoalService interface {
	Evaluate(ctx context.Context, tradeDate string) (*goal.Result, error)
	ConfirmPace(ctx context.Context, tradeDate string) error
	Brief(ctx context.Context, tradeDate string) (notify.GoalBrief, error)
}

// goalBriefOf 取目标概要（邮件顶部"目标还差多少"），失败回落空值不阻断发信。
func goalBriefOf(d Deps, ctx context.Context, date string) notify.GoalBrief {
	if d.Goal == nil {
		return notify.GoalBrief{}
	}
	b, err := d.Goal.Brief(ctx, date)
	if err != nil {
		return notify.GoalBrief{}
	}
	return b
}

// AlertService 告警服务最小接口。
type AlertService interface {
	Raise(ctx context.Context, tradeDate, source string, level model.AlertLevel, code, title, content string) error
	RaiseUrgent(ctx context.Context, tradeDate, source, code, title, content string) error
}

// GoalEvalResult goal.Service.Evaluate 的返回最小视图（接口适配，见 app 装配层）。
type GoalEvalResult struct {
	Gear       string
	FromGear   string
	Changed    bool
	Reason     string
	Progress   float64
	TargetPct  float64
	PaceGapPct float64
	StaleDays  int
}

// BuildJobs 构建完整交易日时间线的 14 个 JobSpec。
func BuildJobs(d Deps) []JobSpec {
	st := d.Store
	today := func(rc *observability.RunCtx) string { return rc.TradeDate() }

	raiseW := func(rc *observability.RunCtx, code, title, content string) {
		if d.Alerts != nil {
			_ = d.Alerts.Raise(rc.Ctx(), today(rc), "scheduler", model.AlertWarning, code, title, content)
		}
	}
	raiseU := func(rc *observability.RunCtx, code, title, content string) {
		if d.Alerts != nil {
			_ = d.Alerts.RaiseUrgent(rc.Ctx(), today(rc), "scheduler", code, title, content)
		}
	}

	return []JobSpec{
		{
			Name: "daily_startup_check", At: []string{"07:00"}, TradeDayOnly: false,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				// 日历覆盖 <30 天 → 续拉（§5.1 晨检）
				rc.Declare("rows", "trade_cal_future_days", -1)
				future, err := st.MarketRepo().CountFutureTradeDays(ctx, date)
				if err != nil {
					return fmt.Errorf("读取日历覆盖失败: %w", err)
				}
				if future < 30 {
					if err := d.Dataloader.SyncCalendar(ctx); err != nil {
						return fmt.Errorf("交易日历续拉失败: %w", err)
					}
					if future, err = st.MarketRepo().CountFutureTradeDays(ctx, date); err != nil {
						return fmt.Errorf("复查日历覆盖失败: %w", err)
					}
				}
				rc.Actual("trade_cal_future_days", future)
				// 昨日未成功任务清单（补跑由调度器按 job_run 自动执行，这里落报告）
				runs, err := st.OpsRepo().ListJobRuns(ctx, prevDate(date))
				if err == nil {
					var unfinished []string
					for _, j := range runs {
						if model.JobStatus(j.Status) == model.JobFailed {
							unfinished = append(unfinished, j.JobName)
						}
					}
					if len(unfinished) > 0 {
						observability.S().Infow("昨日未成功任务（将由调度器补跑）", "jobs", strings.Join(unfinished, ","))
					}
				}
				if d.Mail != nil {
					if missing := d.Mail.HealthOK(); len(missing) > 0 {
						rc.Degrade("MAIL_NOT_CONFIGURED", "邮件配置缺失: "+strings.Join(missing, ","))
					}
				}
				return nil
			},
		},
		{
			Name: "premarket_check", At: []string{"08:30"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				acts, err := st.TradeRepo().ListActiveTickets(ctx, date)
				if err != nil {
					return fmt.Errorf("读取活跃指令单失败: %w", err)
				}
				pos, err := st.TradeRepo().ListPositions(ctx)
				if err != nil {
					return fmt.Errorf("读取持仓失败: %w", err)
				}
				var items []string
				for _, t := range acts {
					if t.IsActive() {
						items = append(items, fmt.Sprintf("有效指令单 #%d %s %s %d 股，有效期至 %s", t.ID, t.TsCode, t.Direction.Label(), int64(t.Qty), t.ValidUntil))
					}
				}
				rp, _, err := d.RiskParams(ctx, date)
				if err == nil {
					for _, p := range pos {
						if p.TotalQty <= 0 {
							continue
						}
						stop := int64(float64(p.CostPrice) * (1 - rp.StopLossPct))
						if p.CostPrice > 0 {
							items = append(items, fmt.Sprintf("持仓 %s 成本 %.2f 元，止损线参考 %.2f 元", p.TsCode, float64(p.CostPrice)/100, float64(stop)/100))
						}
					}
				}
				if len(items) == 0 {
					rc.Degrade("PREMARKET_NO_TRIGGER", "盘前无待关注事项，M2 不发送")
					return nil
				}
				rc.Declare("mail", "premarket_m2", -1)
				subject, body := notify.RenderM2(items, goalBriefOf(d, ctx, date))
				if _, err := d.Mail.Enqueue(ctx, date, model.MailM2, subject, body); err != nil {
					return err
				}
				rc.Actual("premarket_m2", 1)
				if _, err := d.Mail.SendPending(ctx, date); err != nil {
					raiseW(rc, "MAIL_NOT_SENT", "盘前提醒发送失败", err.Error())
				}
				return nil
			},
		},
		{
			Name: "t1_settle", At: []string{"09:25"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				n, err := d.Ledger.SettleT1(ctx, today(rc))
				if err != nil {
					return fmt.Errorf("T+1 结转失败: %w", err)
				}
				rc.Declare("rows", "settled_positions", 0)
				rc.Actual("settled_positions", n)
				return nil
			},
		},
		{
			Name: "intraday_watch", EveryMinutes: 5,
			Windows: []Window{{Start: "09:30", End: "11:30"}, {Start: "13:00", End: "15:00"}},
			TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				codes, err := st.TradeRepo().HoldingCodes(ctx)
				if err != nil {
					return fmt.Errorf("读取持仓代码失败: %w", err)
				}
				if len(codes) == 0 {
					rc.Degrade("NO_HOLDINGS", "无持仓，盘中监控空转")
					return nil
				}
				qs, err := d.Quote.Fetch(ctx, codes)
				if err != nil {
					raiseW(rc, "QUOTE_FETCH_FAILED", "盘中取价失败", err.Error())
					rc.Degrade("QUOTE_FETCH_FAILED", "沿用上一有效价，不据此触发止损")
					return nil
				}
				rc.Declare("quotes", "fetched_codes", len(codes))
				rc.Actual("fetched_codes", len(qs))
				rp, gear, err := d.RiskParams(ctx, date)
				if err != nil {
					return fmt.Errorf("读取生效风控参数失败: %w", err)
				}
				days, err := st.MarketRepo().TradeDateList(ctx)
				if err != nil {
					return fmt.Errorf("读取交易日列表失败: %w", err)
				}
				for _, code := range codes {
					q, ok := qs[code]
					if !ok || q.Price <= 0 {
						continue
					}
					pos, err := st.TradeRepo().GetPosition(ctx, code)
					if err != nil {
						continue
					}
					if pos.TotalQty <= 0 || pos.CostPrice <= 0 {
						continue
					}
					stop := model.Fen(float64(pos.CostPrice) * (1 - rp.StopLossPct))
					if q.Price <= stop {
						// 止损触发：urgent 指令单 + M3 立即发（当日同股只触发一次，靠 urgency 票唯一索引幂等）
						sig := model.Signal{
							TradeDate: date, TsCode: code, Direction: model.DirSell, Rule: "intraday_stop",
							Confidence: 1.0, RefPrice: q.Price,
							Reason: fmt.Sprintf("盘中现价 %.2f ≤ 止损线 %.2f", float64(q.Price)/100, float64(stop)/100),
							Status: "new", CreatedAt: time.Now().UTC().Format(time.RFC3339),
						}
						if _, err := d.Tickets.Create(ctx, sig, pos.AvailableQty, rp, gear, days); err != nil {
							return fmt.Errorf("创建止损指令单 %s 失败: %w", code, err)
						}
						raiseU(rc, "INTRADAY_STOP:"+code+":"+date, "盘中止损触发", sig.Reason)
					}
				}
				return nil
			},
		},
		{
			Name: "expire_overdue", At: []string{"15:00"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				now := time.Now().In(market.Loc)
				expired, err := st.TradeRepo().ListExpiredIssued(ctx, now.Format(time.RFC3339))
				if err != nil {
					return fmt.Errorf("读取逾期指令单失败: %w", err)
				}
				rc.Declare("rows", "expired_tickets", 0)
				n := 0
				for _, t := range expired {
					if err := d.Tickets.Transition(ctx, t.ID, model.TicketExpired, "system", "逾期未执行"); err != nil {
						return fmt.Errorf("过期指令单 #%d 状态流转失败: %w", t.ID, err)
					}
					n++
				}
				rc.Actual("expired_tickets", n)
				return nil
			},
		},
		{
			Name: "data_sync", At: []string{"15:05"}, TradeDayOnly: true,
			Run: makeDataSync(d),
		},
		{
			Name: "snapshot_goal", At: []string{"15:20"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				rep, err := d.Freshness.Check(ctx, date)
				if err != nil {
					return fmt.Errorf("新鲜度门禁执行失败: %w", err)
				}
				if !rep.Fresh {
					raiseU(rc, "SNAPSHOT_SKIPPED", "数据不新鲜，快照跳过", rep.String())
					rc.Degrade("SNAPSHOT_SKIPPED", "不写快照、不插值，目标度量沿用最近快照")
					return nil
				}
				rc.Declare("snapshot", "account_snapshot", -1)
				gear, lock := model.GearG1, false
				if gs, err := st.GoalRepo().GetGoalState(ctx); err == nil && gs.CurrentGear.Valid() {
					gear, lock = gs.CurrentGear, gs.ProfitLock
				}
				if _, err := d.Ledger.TakeSnapshot(ctx, date, gear, lock); err != nil {
					return fmt.Errorf("写入账户快照失败: %w", err)
				}
				rc.Actual("account_snapshot", 1)
			// 季度档位评估（档位变更时 goal 层落 M4 urgent 邮件）
			if d.Goal != nil {
				if _, err := d.Goal.Evaluate(ctx, date); err != nil {
					return fmt.Errorf("季度档位评估失败: %w", err)
				}
			}
			return nil
			},
		},
		{
			Name: "screener", At: []string{"15:30"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				rep, err := d.Screener.Run(ctx, date)
				if err != nil {
					return fmt.Errorf("选股失败: %w", err)
				}
				rc.Declare("rows", "screen_result", -1)
				rc.Actual("screen_result", len(rep.Candidates))
				if rep.Empty {
					// 候选 0：screener 已落 SCREEN_EMPTY urgent + 观察名单；此处显式降级（D1）
					rc.Degrade("SCREEN_EMPTY", "候选 0 条，详见观察名单")
				}
				return nil
			},
		},
		{
			Name: "llm_review", At: []string{"15:40"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				// LLM 终审在 signal 任务内联执行；本任务做配置自检与状态落库。
				rc.Declare("check", "llm_config", -1)
				if c, ok := d.Confirmer.(interface{ Enabled() bool }); ok && !c.Enabled() {
					rc.Actual("llm_config", 1)
					rc.Degrade("LLM_DISABLED", "llm.enabled=false，信号不做 LLM 终审")
					return nil
				}
				rc.Actual("llm_config", 1)
				return nil
			},
		},
		{
			Name: "data_retry_2", At: []string{"15:50"}, TradeDayOnly: true,
			Run: makeDataSync(d),
		},
		{
			Name: "signal", At: []string{"15:50"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				rep2, err := d.Freshness.Check(ctx, date)
				if err != nil {
					return fmt.Errorf("新鲜度门禁执行失败: %w", err)
				}
				if !rep2.Fresh {
					raiseU(rc, "DATA_STALE", "数据不新鲜，不生成任何指令", rep2.String())
					rc.Degrade("DATA_STALE", "信号生成被门禁拦截")
					return nil
				}
				rp, gear, err := d.RiskParams(ctx, date)
				if err != nil {
					return fmt.Errorf("读取生效风控参数失败: %w", err)
				}
				rep, err := d.Signal.Generate(ctx, date, rp, gear, d.Confirmer)
				if err != nil {
					return fmt.Errorf("信号生成失败: %w", err)
				}
				rc.Declare("rows", "signals", -1)
				rc.Actual("signals", rep.Inserted)
				if rep.Empty {
					rc.Degrade("SCREEN_EMPTY", "候选池为空（选股阶段已告警）")
				}
				if rep.Rejected > 0 && rep.Tickets == 0 && rep.BuySignals+rep.SellSignals > 0 {
					raiseU(rc, "ALL_REJECTED", "有信号但全被风控否决", fmt.Sprintf("否决 %d 条", rep.Rejected))
					rc.Degrade("ALL_REJECTED", fmt.Sprintf("否决 %d 条", rep.Rejected))
				}
				return nil
			},
		},
		{
			Name: "ticket_mail", At: []string{"16:10"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				n, err := d.Tickets.IssueAll(ctx, date)
				if err != nil {
					return fmt.Errorf("指令单下发失败: %w", err)
				}
				rc.Declare("mail", "ticket_m1", -1)
				acts, err := st.TradeRepo().ListActiveTickets(ctx, date)
				if err != nil {
					return fmt.Errorf("读取活跃指令单失败: %w", err)
				}
				var lines []notify.TicketLine
				for _, t := range acts {
					if t.Status != model.TicketIssued {
						continue
					}
					lines = append(lines, notify.TicketLine{
						TsCode: t.TsCode, Name: t.Name, Direction: string(t.Direction), DirLabel: t.Direction.Label(),
						Qty: int64(t.Qty), PriceLow: float64(t.RefPriceLow) / 100, PriceHigh: float64(t.RefPriceHigh) / 100,
						ValidUntil: t.ValidUntil, Reason: t.Reason,
					})
				}
				subject, body := notify.RenderM1(lines, goalBriefOf(d, ctx, date),
					fmt.Sprintf("今日无新增有效指令（drafted 下发 %d 张）", n))
				if _, err := d.Mail.Enqueue(ctx, date, model.MailM1, subject, body); err != nil {
					return err
				}
				rc.Actual("ticket_m1", 1)
				if _, err := d.Mail.SendPending(ctx, date); err != nil {
					raiseW(rc, "MAIL_NOT_SENT", "指令邮件发送失败", err.Error())
				}
				return nil
			},
		},
		{
			Name: "data_retry_3", At: []string{"16:30"}, TradeDayOnly: true,
			Run: makeDataSync(d),
		},
		{
			Name: "cleanup", At: []string{"16:40"}, TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				now := time.Now()
				if d.RetentionNow != nil {
					now = d.RetentionNow()
				}
				deletedMap, err := store.ApplyRetention(ctx, st, now, nil)
				if err != nil {
					return fmt.Errorf("保留清理失败: %w", err)
				}
				total := 0
				for _, v := range deletedMap {
					total += v
				}
				rc.Declare("rows", "deleted_rows", 0)
				rc.Actual("deleted_rows", total)
				if err := store.WALCheckpoint(ctx, st); err != nil {
					return fmt.Errorf("WAL checkpoint 失败: %w", err)
				}
				return nil
			},
		},
		{
			Name: "daily_report", At: []string{"20:00"}, TradeDayOnly: false,
			Run: func(ctx context.Context, rc *observability.RunCtx) error {
				date := today(rc)
				rc.Declare("mail", "daily_m5", -1)
				// 先入队 M5（自检的 M5 检查以 outbox 行存在为准），再构建自检区块
				block := BuildDailyBlock(ctx, st, date, d.MinBarRows)
				okJobs, degradedJobs, failedJobs := jobLines(ctx, st, date)
				subject, body := notify.RenderM5(date, block, okJobs, degradedJobs, failedJobs, goalBriefOf(d, ctx, date))
				id, err := d.Mail.Enqueue(ctx, date, model.MailM5, subject, body)
				if err != nil {
					return err
				}
				rc.Actual("daily_m5", 1)
				if err := d.Mail.SendOne(ctx, id); err != nil {
					raiseU(rc, "MAIL_NOT_SENT", "日报发送失败", err.Error())
					return err
				}
				return nil
			},
		},
	}
}

// makeDataSync 数据同步任务体（15:05 / 15:50 / 16:30 三个重试窗口共用；
// 已成功时调度器按 HasJobSucceeded 自动跳过后续窗口）。
func makeDataSync(d Deps) func(ctx context.Context, rc *observability.RunCtx) error {
	return func(ctx context.Context, rc *observability.RunCtx) error {
		date := rc.TradeDate()
		rc.Declare("rows", "daily_bar", -1)
		if err := d.Dataloader.SyncDaily(ctx, date, 10); err != nil {
			if d.Alerts != nil {
				_ = d.Alerts.Raise(ctx, date, "scheduler", model.AlertWarning, "DATA_SYNC_FAILED",
					"数据同步失败（等待下一重试窗口）", err.Error())
			}
			return fmt.Errorf("数据同步失败: %w", err)
		}
		n, err := d.Store.MarketRepo().CountBar(ctx, date)
		if err != nil {
			return fmt.Errorf("核对日线行数失败: %w", err)
		}
		rc.Actual("daily_bar", n)
		return nil
	}
}

// jobLines 日报任务分列：success 一列，degraded 单列（绝不与绿灯混排），failed 一列。
func jobLines(ctx context.Context, st *store.Store, date string) (ok, degraded, failed []string) {
	runs, err := st.OpsRepo().ListJobRuns(ctx, date)
	if err != nil {
		return nil, nil, nil
	}
	for _, j := range runs {
		switch model.JobStatus(j.Status) {
		case model.JobSuccess:
			ok = append(ok, j.JobName)
		case model.JobDegraded:
			degraded = append(degraded, j.JobName)
		case model.JobFailed:
			failed = append(failed, j.JobName)
		}
	}
	return ok, degraded, failed
}

// prevDate 自然日回退一天（晨检读昨日记录用，日期串格式）。
func prevDate(date string) string {
	t, err := time.ParseInLocation("20060102", date, market.Loc)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, -1).Format("20060102")
}
