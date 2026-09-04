package scheduler

// jobs.go 交易日调度表：5 个触发点，每个触发点是一个大方法（顺序组装小方法）。
//
//	09:00  morning_plan     T+1 结转 → 邮件「当日计划 + 持仓」
//	09:30-11:30 / 13:00-15:00  intraday_scan  每 5 分钟扫持仓，跌破止损出卖单
//	16:30  evening_pipeline  同步 → 选股 → LLM 评审 → 写待买卖表 → 次日计划
//	17:00  mail_pending      读待买卖表发邮件，空表不发
//	18:00  daily_report      日报：当天总结 + 持仓 + 次日计划 + 当天失败汇报
//
// 每个大方法的成功/失败由 Scheduler.runJob 统一落 run_trace（一行一个最终结果），
// 大方法内部不再单独落库；失败后不自动重试（当日一个时刻只跑一次）。
//
// 16:30 的流水线不含发信步骤：邮件由 17:00 / 18:00 两个独立任务读表发出，
// 这样流水线即使中断重跑也不会重复发信。保留清理挂在 18:00 日报之后。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
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

// ConfigReader 时刻类配置的读取接口（只读，由组合根注入 config.Config）。
type ConfigReader interface {
	GetString(key string) string
}

// Deps 任务依赖集合（由 app.Runtime.SchedDeps 装配注入）。
type Deps struct {
	Store        *store.Store
	Dataloader   *dataloader.Dataloader
	Freshness    *dataloader.FreshnessGate
	Quote        *quote.GotdxSource
	Screener     *screener.Screener
	Signal       *signal.Service
	Decider      signal.BuyDecider
	Ledger       *ticket.Ledger
	Tickets      *ticket.Service
	Goal         GoalService
	Alerts       AlertService
	Mail         *notify.Mailer
	RiskParams   func(ctx context.Context, date string) (risk.RiskParams, model.Gear, error)
	FilterCfg    screener.FilterConfig
	MinBarRows   int
	RetentionNow func() time.Time
	Config       ConfigReader   // scheduler.* 触发时刻
	Retention    map[string]int // retention.* 保留窗口覆盖（空=用规则默认值）
}

// at 读取一个 scheduler.* 时刻键，逗号分隔即多个触发时刻。
// 缺省值只在 config.KeySpec 里写一次，这里不再重复字面量。
func (d Deps) at(key string) []string {
	var out []string
	for _, p := range strings.Split(d.Config.GetString(key), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// GoalService 目标服务最小接口。
type GoalService interface {
	Evaluate(ctx context.Context, tradeDate string) (*goal.Result, error)
	ConfirmPace(ctx context.Context, tradeDate string) error
	Brief(ctx context.Context, tradeDate string) (notify.GoalBrief, error)
}

// AlertService 告警服务最小接口。
type AlertService interface {
	Raise(ctx context.Context, tradeDate, source string, level model.AlertLevel, code, title, content string) error
	RaiseUrgent(ctx context.Context, tradeDate, source, code, title, content string) error
}

// raiseW / raiseU 落 warning / urgent 告警。告警写失败必须留痕：
// run_trace 是对外唯一的异常出口，它自己砸了如果没有日志，就等于什么都没发生。
func (d Deps) raiseW(rc *observability.RunCtx, code, title, content string) {
	if err := d.Alerts.Raise(rc.Ctx(), rc.TradeDate(), "scheduler", model.AlertWarning, code, title, content); err != nil {
		observability.S().Errorw("写 warning 告警失败", "code", code, "err", err.Error())
	}
}

func (d Deps) raiseU(rc *observability.RunCtx, code, title, content string) {
	if err := d.Alerts.RaiseUrgent(rc.Ctx(), rc.TradeDate(), "scheduler", code, title, content); err != nil {
		observability.S().Errorw("写 urgent 告警失败", "code", code, "err", err.Error())
	}
}

// goalBriefOf 取目标概要（邮件顶部"目标还差多少"）。读不到就返回错误：
// 空概要会把邮件里的总资产/现金渲染成 0.00 元，那是假数据而不是缺省值。
func goalBriefOf(d Deps, ctx context.Context, date string) (notify.GoalBrief, error) {
	b, err := d.Goal.Brief(ctx, date)
	if err != nil {
		return notify.GoalBrief{}, fmt.Errorf("goal.Brief(%s): %w", date, err)
	}
	return b, nil
}

// BuildJobs 构建 5 个触发点。顺序即同一时刻的执行次序。
func BuildJobs(d Deps) []JobSpec {
	return []JobSpec{
		{
			Name: "morning_plan", At: d.at("scheduler.morning"), TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error { return morningPlan(ctx, rc, d) },
		},
		{
			Name: "intraday_scan", EveryMinutes: 5,
			Windows:      []Window{{Start: "09:30", End: "11:30"}, {Start: "13:00", End: "15:00"}},
			TradeDayOnly: true,
			Run:          func(ctx context.Context, rc *observability.RunCtx) error { return intradayScan(ctx, rc, d) },
		},
		{
			Name: "evening_pipeline", At: d.at("scheduler.pipeline"), TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error { return eveningPipeline(ctx, rc, d) },
		},
		{
			Name: "mail_pending", At: d.at("scheduler.mail_pending"), TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error { return mailPending(ctx, rc, d) },
		},
		{
			Name: "daily_report", At: d.at("scheduler.report"), TradeDayOnly: true,
			Run: func(ctx context.Context, rc *observability.RunCtx) error { return dailyReport(ctx, rc, d) },
		},
	}
}

// jobLines 日报的任务分列：ok 一列，partial 单列（绝不与绿灯混排），fail 一列。
// 读轨迹失败必须上抛——空列表会被读成"今天零任务"，那是假绿。
func jobLines(ctx context.Context, st *store.Store, date string) (ok, degraded, failed []string, err error) {
	traces, err := st.TraceRepo().List(ctx, date)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("读取 %s 任务轨迹失败: %w", date, err)
	}
	for _, t := range traces {
		name, isJob := strings.CutPrefix(t.Subject, "job:")
		if !isJob {
			continue
		}
		switch t.Outcome {
		case model.TraceOK:
			ok = append(ok, name)
		case model.TracePartial:
			degraded = append(degraded, name)
		case model.TraceFail:
			failed = append(failed, name)
		}
	}
	return ok, degraded, failed, nil
}

// fmtYuan 分 → 元展示串（邮件正文统一口径）。
func fmtYuan(fen int64) string { return fmt.Sprintf("%.2f", float64(fen)/100) }
