package app

// 单进程常驻的运行时装配：调度器与 MCP 接口共用同一套服务实例，
// 保证"后台任务算出来的"与"agent 读到的"永远是同一份状态。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/mcp"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/scheduler"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
	"jingzhe-trader/internal/tushare"
)

// Runtime 运行时服务集合（一个进程内同时供调度器与 MCP 使用）。
type Runtime struct {
	Store      *store.Store
	Config     *config.Config
	Tushare    *tushare.Client
	Quote      *quote.GotdxSource
	Dataloader *dataloader.Dataloader
	Freshness  *dataloader.FreshnessGate
	Screener   *screener.Screener
	Signal     *signal.Service
	Decider    signal.BuyDecider
	Ledger     *ticket.Ledger
	Tickets    *ticket.Service
	Goal       *goal.Service
	Mail       *notify.Mailer
	Alerts     *notify.AlertService
	MinBarRows int

	sched *scheduler.Scheduler
}

// BuildRuntime 装配全部下游服务。
func BuildRuntime(ctx context.Context, st *store.Store, cfg *config.Config) (*Runtime, error) {
	if st == nil {
		return nil, fmt.Errorf("app.BuildRuntime: Store 不能为空")
	}
	if cfg == nil {
		return nil, fmt.Errorf("app.BuildRuntime: Config 不能为空")
	}

	ic, err := InitialCapitalOf(cfg)
	if err != nil {
		return nil, fmt.Errorf("app.BuildRuntime: %w", err)
	}
	if err := validateEnums(cfg); err != nil {
		return nil, fmt.Errorf("app.BuildRuntime: %w", err)
	}
	tcli := tushare.NewClient(
		cfg.GetString("tushare.token"),
		cfg.GetString("tushare.base_url"),
		cfg.GetInt("tushare.rate_per_min"),
	)
	mail := notify.NewMailer(st, MailConfigOf(cfg))
	// 邮件是 M1/M2/M3/M5 的交付物本身：配置不全就在装配期失败，
	// 让它变成每天一条"降级"的代价是整天没有任何通知而任务全绿。
	if missing := mail.HealthOK(); len(missing) > 0 {
		return nil, fmt.Errorf("app.BuildRuntime: 邮件配置不完整: %v", missing)
	}
	alerts := notify.NewAlertService(st, mail)

	// 买入决策者就是 LLM 本身（用户拍板：LLM 决定标的与数量，风控只做硬截断）。
	// 未启用时不回落成"规则说了算"—— 那等于把刚拆掉的均线/综合分门槛装回来。
	llmClient := llm.NewClient(cfg.GetString("llm.api_key"), cfg.GetString("llm.base_url"),
		cfg.GetString("llm.model"), cfg.GetString("llm.search_context_size"), nil)
	decider := llm.NewReviewer(llmClient, st, cfg.GetBool("llm.enabled"))

	cost := CostParamsOf(cfg)
	ledger := ticket.NewLedger(st, cost, ic)
	rt := &Runtime{
		Store:      st,
		Config:     cfg,
		Tushare:    tcli,
		Quote:      quote.NewGotdxSource(),
		Dataloader: dataloader.New(st, tcli),
		Freshness:  dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), screener.BarWindow()),
		Screener:   screener.New(st, FilterConfigOf(cfg)),
		Signal:     signal.NewService(st, ledger),
		Decider:    decider,
		Ledger:     ledger,
		Tickets:    ticket.NewService(st),
		// WithAlertFunc 必须接上：PACE_BOOST_DENIED / EXPIRED 是设计上"人必须知道"的事件，
		// 回调留 nil 就等于这条告警永远发不出去（那正是装配期的静默）。
		Goal:       goal.NewService(st, GoalConfigOf(cfg), ledger).WithAlertFunc(alerts.Raise),
		Mail:       mail,
		Alerts:     alerts,
		MinBarRows: cfg.GetInt("screen.min_bar_rows"),
	}
	// 调度器归 Runtime 持有：常驻的调度循环与 /healthz 探活看的是同一个实例。
	rt.sched = scheduler.New(st,
		func(date string) bool { return rt.IsTradeDay(ctx, date) },
		scheduler.BuildJobs(rt.SchedDeps()))
	return rt, nil
}

// Scheduler 返回本进程唯一的调度器实例（构造期已装配，With* 可直接链式覆写）。
func (r *Runtime) Scheduler() *scheduler.Scheduler { return r.sched }

// SchedDeps 映射为调度器依赖（scheduler.BuildJobs 的入参）。
func (r *Runtime) SchedDeps() scheduler.Deps {
	return scheduler.Deps{
		Store:        r.Store,
		Dataloader:   r.Dataloader,
		Freshness:    r.Freshness,
		Quote:        r.Quote,
		Screener:     r.Screener,
		Signal:       r.Signal,
		Decider:      r.Decider,
		Ledger:       r.Ledger,
		Tickets:      r.Tickets,
		Goal:         r.Goal,
		Alerts:       r.Alerts,
		Mail:         r.Mail,
		RiskParams:   r.Goal.RiskParams,
		FilterCfg:    FilterConfigOf(r.Config),
		MinBarRows:   r.MinBarRows,
		RetentionNow: time.Now,
		Config:       r.Config,
		Retention:    RetentionOverridesOf(r.Config),
	}
}

// MCPDeps 映射为 MCP 工具依赖（mcp.New 的入参）：与调度器共用同一批实例。
func (r *Runtime) MCPDeps() mcp.Deps {
	return mcp.Deps{
		Store:      r.Store,
		Config:     r.Config,
		Ledger:     r.Ledger,
		Tickets:    r.Tickets,
		Goal:       r.Goal,
		Freshness:  r.Freshness,
		Liveness:   r.sched,
		Jobs:       r.sched,
		MinBarRows: r.MinBarRows,
	}
}

// IsTradeDay 交易日判定（读 trade_cal 表）。
//
// 日历缺当天行时按"是交易日"继续，但这不是静默兜底：真正的拦网是新鲜度门禁的
// CalendarOK（阻断项），缺日历当日就出不了指令。反过来静默不跑，
// 一整天不会留下任何失败痕迹——那才是 [D1] 抓的那种假正常。
func (r *Runtime) IsTradeDay(ctx context.Context, date string) bool {
	cal, err := r.Store.MarketRepo().LoadTradeCal(ctx)
	if err != nil {
		observability.S().Errorw("读取交易日历失败，按交易日继续（门禁 CalendarOK 会拦下当日）",
			"date", date, "err", err.Error())
		return true
	}
	if _, ok := cal[date]; !ok {
		observability.S().Warnw("交易日历缺当天行，按交易日继续（门禁 CalendarOK 会拦下当日）",
			"date", date, "cal_rows", len(cal))
	}
	return market.IsTradeDay(cal, date)
}

// ===================== 配置 → 领域参数 =====================

// FilterConfigOf 从配置构建粗筛参数。
//
// 不写 "if v > 0" 式的覆盖：默认值只在 config.KeySpec 里有一份，
// 这里再兜一次会让"显式配成 0"静默失效，同一阈值出现两处真相。
func FilterConfigOf(cfg *config.Config) screener.FilterConfig {
	return screener.FilterConfig{
		TopN:             cfg.GetInt("screen.top_n"),
		MinCircMvW:       cfg.GetFloat("screen.min_circ_mv_w"),
		MinTurnoverRate:  cfg.GetFloat("screen.min_turnover_rate"),
		PriceLow:         cfg.GetFloat("screen.price_low"),
		MinListDays:      cfg.GetInt("screen.min_list_days"),
		SectorTopK:       cfg.GetInt("screen.sector_top_k"),
		MinSectorMembers: cfg.GetInt("screen.min_sector_members"),
		PETtmMax:         cfg.GetFloat("screen.pe_ttm_max"),
		PBMax:            cfg.GetFloat("screen.pb_max"),
	}
}

// CostParamsOf 交易成本参数（config cost.* 键）。
func CostParamsOf(cfg *config.Config) market.CostParams {
	return market.CostParams{
		CommissionRate:  cfg.GetFloat("cost.commission_rate"),
		MinCommission:   model.FromFloat(cfg.GetFloat("cost.min_commission")),
		StampTaxRate:    cfg.GetFloat("cost.stamp_tax_rate"),
		TransferFeeRate: cfg.GetFloat("cost.transfer_fee_rate"),
	}
}

// validateEnums 枚举类配置在装配期严格校验：拼错一个字母就"认不出来用默认"，
// 等于把一个看得见配置项变成掷骰子。
func validateEnums(cfg *config.Config) error {
	switch cfg.GetString("goal.pace_policy") {
	case "unrestricted", "conservative", "aggressive":
	default:
		return fmt.Errorf("goal.pace_policy=%q 非法（可选 unrestricted|conservative|aggressive）",
			cfg.GetString("goal.pace_policy"))
	}
	switch cfg.GetString("llm.search_context_size") {
	case "low", "medium", "high":
	default:
		return fmt.Errorf("llm.search_context_size=%q 非法（可选 low|medium|high）",
			cfg.GetString("llm.search_context_size"))
	}
	// 触发时刻拼错时调度器只在每次 tick 记一条日志、整天不跑这个任务；装配期直接拒绝。
	for _, key := range []string{"scheduler.morning", "scheduler.pipeline", "scheduler.mail_pending", "scheduler.report"} {
		for _, hm := range strings.Split(cfg.GetString(key), ",") {
			hm = strings.TrimSpace(hm)
			if hm == "" {
				continue
			}
			if _, err := time.ParseInLocation("15:04", hm, market.Loc); err != nil {
				return fmt.Errorf("%s=%q 不是 HH:MM: %w", key, hm, err)
			}
		}
	}
	return nil
}

// RetentionOverridesOf 保留窗口覆盖：键名取自 store.RetentionRules 声明（不再抄一份清单），
// 仅正数配置生效，未配置的表沿用规则默认窗口。
func RetentionOverridesOf(cfg *config.Config) map[string]int {
	out := make(map[string]int, len(store.RetentionRules))
	for _, r := range store.RetentionRules {
		if r.ConfigKey == "" {
			continue
		}
		if v := cfg.GetInt(r.ConfigKey); v > 0 {
			out[r.ConfigKey] = v
		}
	}
	return out
}

// GoalConfigOf 由全局 config 拼装目标域配置。
//
// 季度基准不在这里给：它由 goal 在首次评估时按实时总资产写入，之后一直用状态里那一份。
func GoalConfigOf(cfg *config.Config) goal.Config {
	return goal.Config{
		TargetPct: cfg.GetFloat("goal.quarterly_target_pct"),
		BudgetPct: cfg.GetFloat("goal.max_drawdown_budget"),
		Pace: goal.PaceSettings{
			Policy:      cfg.GetString("goal.pace_policy"),
			MaxBoostPct: cfg.GetFloat("goal.pace_max_boost_pct"),
			BudgetBelow: cfg.GetFloat("goal.pace_allow_if_budget_below"),
		},
		MaxSectorPct:  cfg.GetFloat("risk.max_sector_pct"),
		TakeProfitPct: cfg.GetFloat("risk.take_profit_pct"),
		Gear: goal.GearConfig{
			TightenAtBudget:   cfg.GetFloat("goal.tighten_at_budget"),
			DefendAtBudget:    cfg.GetFloat("goal.defend_at_budget"),
			UpgradeHysteresis: cfg.GetFloat("goal.upgrade_hysteresis"),
			UpgradeDays:       cfg.GetInt("goal.upgrade_days"),
			LockAtProgress:    cfg.GetFloat("goal.lock_at_progress"),
			LockBudgetBelow:   cfg.GetFloat("goal.lock_budget_below"),
		},
	}
}

// RunTaskOnce 立即执行一个已注册任务一次：CLI 手工跑、MCP trigger_task 与调度器到点触发
// 走同一条 runJob 路径（同一条 run_trace 落库口径），避免出现第二套执行逻辑。
func (r *Runtime) RunTaskOnce(ctx context.Context, name, date, trigger string) error {
	if r.sched == nil {
		return fmt.Errorf("调度器未装配，无法执行任务 %s", name)
	}
	return r.sched.RunNamed(ctx, name, date, trigger)
}

// TaskNames 已注册任务名（供 CLI 与外部 agent 校验入参）。
func (r *Runtime) TaskNames() []string {
	if r.sched == nil {
		return nil
	}
	return r.sched.JobNames()
}

// InitialCapitalOf 本金（config account.initial_capital，单位元）。
//
// 没有回落值：本金是现金推算、档位与仓位的共同基线，编一个默认值等于
// 让整天的仓位计算建立在一个没人说过的数字上。真实基线由 init / sync_portfolio 写入。
func InitialCapitalOf(cfg *config.Config) (model.Fen, error) {
	v := cfg.GetFloat("account.initial_capital")
	if v <= 0 {
		return 0, fmt.Errorf("本金未初始化：先跑 jingzhe init -capital <元>（键 account.initial_capital）")
	}
	return model.FromFloat(v), nil
}

// MailConfigOf 由全局 config 拼装邮件配置；收件人取自 watch.mail_to（逗号分隔）。
func MailConfigOf(cfg *config.Config) notify.MailConfig {
	var to []string
	for _, s := range strings.Split(cfg.GetString("watch.mail_to"), ",") {
		if t := strings.TrimSpace(s); t != "" {
			to = append(to, t)
		}
	}
	return notify.MailConfig{
		Enabled: cfg.GetBool("mail.enabled"),
		SMTP: notify.SMTPConfig{
			Host:     cfg.GetString("mail.smtp_host"),
			Port:     cfg.GetInt("mail.smtp_port"),
			From:     cfg.GetString("mail.from"),
			Password: cfg.GetString("mail.password"),
		},
		To: to,
	}
}
