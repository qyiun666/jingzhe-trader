package app

// 单进程常驻的运行时装配：调度器与 MCP 接口共用同一套服务实例，
// 保证"后台任务算出来的"与"agent 读到的"永远是同一份状态。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/mcp"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/risk"
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
	// 评审链的整轮/单条时限与批大小由配置注入（agent_issues #14）：旧形状是编译期常量
	// 12min 整轮 + 10min 单条，任一证据 prompt 用满就吃光整轮预算、第 2 次重问必然
	// 看见 ctx 已 Done。装配期已校验三键范围与"单条 < 整轮"，流水线任务级超时
	// 也随整轮预算自动扩（scheduler.pipelineJobTimeout），三者不再各自为政。
	// 批大小 n≤0 时 llm 侧保持"整批一次问"的历史行为，这里不做二次解释。
	decider := llm.NewReviewer(llmClient, st, cfg.GetBool("llm.enabled")).
		WithTimeouts(time.Duration(cfg.GetInt("llm.review_deadline_min"))*time.Minute,
			time.Duration(cfg.GetInt("llm.review_prompt_timeout_min"))*time.Minute).
		WithBatchSize(cfg.GetInt("llm.review_batch_size"))

	cost := CostParamsOf(cfg)
	ledger := ticket.NewLedger(st, cost, ic)
	rt := &Runtime{
		Store:      st,
		Config:     cfg,
		Tushare:    tcli,
		Quote:      quote.NewGotdxSource(),
		Dataloader: dataloader.New(st, tcli),
		Freshness:  dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows"), screener.BarWindow()),
		Screener:   screener.New(st, FilterConfigOf(cfg), cfg.GetString("screen.factor_mode")),
		Signal: signal.NewService(st, ledger, scheduler.MarketGateOf(cfg), scheduler.ETFUniverseOf(cfg),
			scheduler.RankOutPolicyOf(cfg)).WithWeakExit(scheduler.WeakExitPolicyOf(cfg)),
		Decider:    decider,
		Ledger:     ledger,
		Tickets:    ticket.NewService(st),
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
		Alerts:       r.Alerts,
		Mail:         r.Mail,
		RiskParams:   RiskParamsOf(r),
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
		BufferN:          cfg.GetInt("signal.rank_out_buffer_n"),
		MinCircMvW:       cfg.GetFloat("screen.min_circ_mv_w"),
		MinTurnoverRate:  cfg.GetFloat("screen.min_turnover_rate"),
		PriceLow:         cfg.GetFloat("screen.price_low"),
		MinListDays:      cfg.GetInt("screen.min_list_days"),
		SectorTopK:       cfg.GetInt("screen.sector_top_k"),
		MinSectorMembers: cfg.GetInt("screen.min_sector_members"),
		PETtmMax:         cfg.GetFloat("screen.pe_ttm_max"),
		PBMax:            cfg.GetFloat("screen.pb_max"),
		MaxPerIndustry:   cfg.GetInt("screen.max_per_industry"),
		SectorShadow:     cfg.GetBool("screen.sector_shadow"),
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
	switch cfg.GetString("llm.search_context_size") {
	case "low", "medium", "high":
	default:
		return fmt.Errorf("llm.search_context_size=%q 非法（可选 low|medium|high）",
			cfg.GetString("llm.search_context_size"))
	}
	if !screener.ValidFactorMode(cfg.GetString("screen.factor_mode")) {
		return fmt.Errorf("screen.factor_mode=%q 非法（可选 %s|%s）",
			cfg.GetString("screen.factor_mode"), screener.ModeMomentum, screener.ModeReversal)
	}
	// 大盘门槛窗口在装配期卡死在 [1, store.MarketMAWindow]：超过同步保证的深度时
	// 均线永远不可算，等于把"可配置"变成"每天定时整链失败"的配置。
	if w := cfg.GetInt("screen.gate_ma_window"); w < 1 || w > store.MarketMAWindow {
		return fmt.Errorf("screen.gate_ma_window=%d 非法（合法 1..%d，超过部分指数日线本就不回补）",
			w, store.MarketMAWindow)
	}
	// 弱势试探上限同样装配期卡死：越界值运行期虽会被 gateOffCapsOf/WeakParams 钳回，
	// 但那等于"配置写错了系统悄悄替你做主"——和掷骰子一样该在启动时就拦下。
	total := cfg.GetFloat("screen.gate_off_max_total_pct")
	single := cfg.GetFloat("screen.gate_off_max_single_pct")
	if total <= 0 || total > risk.CircuitMaxTotalPct {
		return fmt.Errorf("screen.gate_off_max_total_pct=%g 非法（合法 0..%.2f，物理熔断上限）",
			total, risk.CircuitMaxTotalPct)
	}
	if single <= 0 || single > risk.CircuitMaxSinglePct {
		return fmt.Errorf("screen.gate_off_max_single_pct=%g 非法（合法 0..%.2f，物理熔断上限）",
			single, risk.CircuitMaxSinglePct)
	}
	if single > total {
		return fmt.Errorf("screen.gate_off_max_single_pct=%g 大于总仓上限 %g（单票不可能超过总仓）",
			single, total)
	}
	// 移动止盈激活线：0 合法（回到旧口径），越界拒绝启动。用原始串解析而不是
	// GetFloat，避免手工构造的局部 Config 缺键时被当成 0 静默放行。
	if raw := strings.TrimSpace(cfg.GetString("risk.trailing_arm_pct")); raw != "" {
		arm, err := strconv.ParseFloat(raw, 64)
		if err != nil || arm < 0 || arm > 0.5 {
			return fmt.Errorf("risk.trailing_arm_pct=%q 非法（合法 0..0.5，0=旧口径）", raw)
		}
	}
	// 排名淘汰的持有期/缓冲：0 合法（单独关掉一项保护回到旧口径），负值非法——
	// 负持有期会把"淘汰前至少等 N 天"翻转成"淘汰可以倒着数"，语义不存在。
	if v := cfg.GetInt("signal.rank_out_min_hold_days"); v < 0 {
		return fmt.Errorf("signal.rank_out_min_hold_days=%d 非法（合法 ≥0，0=不设最小持有期）", v)
	}
	if v := cfg.GetInt("signal.rank_out_buffer_n"); v < 0 {
		return fmt.Errorf("signal.rank_out_buffer_n=%d 非法（合法 ≥0，0=保留带与 TopN 等深）", v)
	}
	// 2026-11-03 定参的三个买入侧开关/阈值。布尔两键单独再解析一遍：GetBool 对拼错的
	// 值按 false 处理，而这里 false 与 true 是相反的政策（停买 vs 试探买），
	// 静默落错边等于"配置写错了系统替你做主还不吭声"。
	for _, key := range []string{"signal.weak_buy_enabled", "signal.rank_out_weak_enabled"} {
		if raw := strings.TrimSpace(cfg.GetString(key)); raw != "" {
			if _, err := strconv.ParseBool(raw); err != nil {
				return fmt.Errorf("%s=%q 非法（合法 true|false）", key, raw)
			}
		}
	}
	// 摩擦闸的预估持有期超额。0 是**默认且有意的值**（无显著归因证据 = 全拦），
	// 所以这里不设 RefuseZero；上限 0.2 挡的是把百分数当小数写（20 而不是 0.20）
	// ——那种量纲事故的后果是"闸永久放行"，比关掉闸更难被发现。
	if raw := strings.TrimSpace(cfg.GetString("signal.expected_excess_pct")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 0.2 {
			return fmt.Errorf("signal.expected_excess_pct=%q 非法（合法 0..0.2 的小数；0=无显著证据即全拦，百分数请写成 0.005 而不是 0.5）", raw)
		}
	}
	// 动态武装线的安全垫：0 合法（只取静态下限），超过 10% 会把武装线推到弱势档
	// 单票上限之上，等于止盈永不武装——那应该显式关掉它，不是靠一个大数凑出来。
	if raw := strings.TrimSpace(cfg.GetString("risk.trailing_arm_safety_pct")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 0.1 {
			return fmt.Errorf("risk.trailing_arm_safety_pct=%q 非法（合法 0..0.1）", raw)
		}
	}
	// 转强防抖天数：0/1 = 即时转强（旧行为可回退）；上限 10 天，更长等于事实上
	// 永不转强，这种事应当由人显式决定，不由一个手滑的数字决定。
	if raw := strings.TrimSpace(cfg.GetString("screen.gate_confirm_days")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > 10 {
			return fmt.Errorf("screen.gate_confirm_days=%q 非法（合法 0..10，0/1=即时转强）", raw)
		}
	}
	// 评审链三个预算键（agent_issues #14）。单条时限必须**严格小于**整轮：
	// 10-09/10-30 两次事故的形状就是单条 10min 贴着整轮 12min，任一条用满就吃光
	// 整轮预算，第 2 次重问必然看见 ctx 已 Done。llm.WithTimeouts 对越界是静默忽略，
	// 那等于把事故原因藏在被忽略的配置里——所以同一条判据挪到装配期，直接拒绝启动。
	reviewDeadline := 0
	if raw := strings.TrimSpace(cfg.GetString("llm.review_deadline_min")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 60 {
			return fmt.Errorf("llm.review_deadline_min=%q 非法（合法 1..60 分钟）", raw)
		}
		reviewDeadline = v
	}
	if raw := strings.TrimSpace(cfg.GetString("llm.review_prompt_timeout_min")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 60 {
			return fmt.Errorf("llm.review_prompt_timeout_min=%q 非法（合法 1..60 分钟）", raw)
		}
		if reviewDeadline > 0 && v >= reviewDeadline {
			return fmt.Errorf("llm.review_prompt_timeout_min=%d 不小于 llm.review_deadline_min=%d（单条必须小于整轮，否则重问永远等不到自己的超时）", v, reviewDeadline)
		}
	}
	if raw := strings.TrimSpace(cfg.GetString("llm.review_batch_size")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 50 {
			return fmt.Errorf("llm.review_batch_size=%q 非法（合法 1..50 只/批）", raw)
		}
	}
	// 弱势减仓：只减不清是硬语义，reduce_pct 必须严格小于 1；跌幅阈值 0 会让
	// 任何一天的小跌都触发减仓，也不是有效配置。只校验显式配置过的键，
	// 局部构造的 Config（测试/CLI 片段）缺键时保留 KeySpec 默认档。
	if raw := strings.TrimSpace(cfg.GetString("signal.weak_exit_enabled")); raw != "" {
		if _, err := strconv.ParseBool(raw); err != nil {
			return fmt.Errorf("signal.weak_exit_enabled=%q 非法（合法 true|false）", raw)
		}
	}
	if raw := strings.TrimSpace(cfg.GetString("signal.weak_exit_drop_pct")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 || v > 0.2 {
			return fmt.Errorf("signal.weak_exit_drop_pct=%q 非法（合法 0<v≤0.2）", raw)
		}
	}
	if raw := strings.TrimSpace(cfg.GetString("signal.weak_exit_reduce_pct")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 || v >= 1 {
			return fmt.Errorf("signal.weak_exit_reduce_pct=%q 非法（合法 0<v<1，只减不清）", raw)
		}
	}
	// 行业集中度：0=关闭，上限 10（再高对 20 只的候选池等于没限）。
	if raw := strings.TrimSpace(cfg.GetString("screen.max_per_industry")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 || v > 10 {
			return fmt.Errorf("screen.max_per_industry=%q 非法（合法 0..10，0=关闭）", raw)
		}
	}
	if err := validateETF(cfg); err != nil {
		return err
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

// RiskParamsOf 由组合根提供生效风控参数的读取函数：总资产取自账本实时推算，
// 其余为 risk.DefaultParams 的固定基准 + config 覆盖项（止盈线）。
//
// 调度器与 MCP 共用这一份，手工补跑与到点自动跑的风控参数因此完全同源。
func RiskParamsOf(r *Runtime) func(ctx context.Context, date string) (risk.RiskParams, error) {
	return func(ctx context.Context, date string) (risk.RiskParams, error) {
		ast, err := r.Ledger.Assets(ctx, date)
		if err != nil {
			return risk.RiskParams{}, fmt.Errorf("读取账户资产失败，风控参数不可算: %w", err)
		}
		p := risk.DefaultParams(ast.TotalAsset)
		if tp := r.Config.GetFloat("risk.take_profit_pct"); tp > 0 {
			p.TakeProfitPct = tp
		}
		// 键缺失时保留 DefaultParams 的默认值；显式写 0 才回滚旧口径。
		// 与 validateEnums 同一判据：原始串解析失败/越界一律报错，不静默用 0。
		if raw := strings.TrimSpace(r.Config.GetString("risk.trailing_arm_pct")); raw != "" {
			arm, err := strconv.ParseFloat(raw, 64)
			if err != nil || arm < 0 || arm > 0.5 {
				return risk.RiskParams{}, fmt.Errorf("risk.trailing_arm_pct=%q 非法（合法 0..0.5，0=旧口径）", raw)
			}
			p.TrailingArmPct = arm
		}
		// 买入侧行业上限复用 screen.max_per_industry：默认 2（配置 3 只候选上限
		// 不会让账户同行业持有 3 只），0=关闭，配置收紧到 1 时跟随收紧。
		// 键缺失（局部构造的 Config）时保留 DefaultParams 的 2，不静默关闭。
		if raw := strings.TrimSpace(r.Config.GetString("screen.max_per_industry")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				p.MaxPerIndustry = risk.BuyIndustryCap(n)
			}
		}
		return p, nil
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
// 没有回落值：本金是现金推算与仓位的共同基线，编一个默认值等于
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

// ETFWhitelistCodes 解析 screen.etf_whitelist 的代码列（格式 code[:name[:track]]）。
//
// 切分规则只有一份，长在 screener 那边（分隔符集合、三段式、裸六位代码补交易所后缀、
// 大小写归一与去重）：漏斗侧与同步侧读到不同的代码集时，症状是"日线明明同步了
// 却整天窗口日线不足"，是最难归因的一类。依赖方向是 app → screener（组合根本来就
// 依赖选股包），不构成环。这里保留 error 返回值，是为了让 run task etf 这类
// 动手前先校验的调用方把非法配置当场喊停。
func ETFWhitelistCodes(raw string) ([]string, error) {
	if err := screener.ValidateETFWhitelist(raw, false); err != nil {
		return nil, err
	}
	return screener.ETFWhitelistCodes(raw), nil
}

// validateETF ETF 补口的装配期校验。
//
// 空串按 KeySpec 默认值处理（关 / 2.0 亿）：那是"键还没落库"的形态，不是坏值——
// 真实运行期由 config.Load 兜默认，只有手工拼装 Config 的测试与旧库会走到这里。
// 一旦显式写了值，就必须严格：非法布尔、非正阈值、代码格式错、重复代码统统拒绝启动，
// 因为 ETF 这条链的失败形态是"静默 0 候选"（代码写错时 fund_daily 只返回空），
// 那种错在运行期永远看不见，只能在启动那一下拦。
//
// 只在总闸打开时严格要求白名单可用：关掉时（默认）这是一份"还没启用的配置"，
// 里面缺个名称不该让今天的服务起不来；打开时缺名称就意味着待发的那张指令单上
// 标的名称是空的——那必须拒绝启动，而不是运行期悄悄少一只候选。
// 白名单那一段的严格判据与选股侧共用同一份实现（screener.ValidateETFWhitelist）。
func validateETF(cfg *config.Config) error {
	rawEnabled := strings.TrimSpace(cfg.GetString("screen.etf_enabled"))
	enabled := false
	if rawEnabled != "" {
		v, err := strconv.ParseBool(rawEnabled)
		if err != nil {
			return fmt.Errorf("screen.etf_enabled=%q 不是布尔值（可选 true|false）", rawEnabled)
		}
		enabled = v
	}
	if raw := strings.TrimSpace(cfg.GetString("screen.etf_min_avg_amount_yi")); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v <= 0 {
			return fmt.Errorf("screen.etf_min_avg_amount_yi=%q 非法（须为正数，单位亿元；设 0 等于取消流动性这一级）", raw)
		}
	}
	return screener.ValidateETFWhitelist(cfg.GetString("screen.etf_whitelist"), enabled)
}
