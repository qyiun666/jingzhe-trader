package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// MarketGateOf 由配置构建大盘门槛口径（组合根与 CLI 手工试跑共用，同一份判据）。
// 只走 ConfigReader 的字符串读取：解析失败的坏值在装配期被启动自检拦下（malformed），
// 这里兜底的默认值是给"键缺失的旧库"用的——窗口越界同样按深度上限钳回，
// 宁可按默认深度算，也不让非法窗口把整条链炸在运行中。
func MarketGateOf(cfg ConfigReader) store.MarketGate {
	w, err := strconv.Atoi(strings.TrimSpace(cfg.GetString("screen.gate_ma_window")))
	if err != nil || w < 1 || w > store.MarketMAWindow {
		w = store.MarketMAWindow
	}
	enabled, err := strconv.ParseBool(strings.TrimSpace(cfg.GetString("screen.gate_enabled")))
	if err != nil {
		enabled = true
	}
	return store.MarketGate{Enabled: enabled, Window: w}
}

// eveningPipeline 收盘后整链大方法：一条顺序流水线，任一步失败即整链失败（落 run_trace outcome=fail）。
//
// ① 行情同步 → ② 新鲜度门禁 → ③ 选股漏斗 → ④ 买卖决策 → ⑤ 写待买卖表。
// 只有 ①⑤ 写库：行情是缓存、指令单是结果；③④ 的中间产物全部在内存里传递，
// 每级进出计数写日志。
func eveningPipeline(ctx context.Context, rc *observability.RunCtx, d Deps) error {
	date := rc.TradeDate()
	if err := syncTodayBars(ctx, rc, d, date); err != nil {
		return fmt.Errorf("① 行情同步: %w", err)
	}
	if err := gateFreshness(ctx, d, date); err != nil {
		d.raiseU(rc, "DATA_STALE", "数据不新鲜，当日不出指令", err.Error())
		return fmt.Errorf("② 新鲜度门禁: %w", err)
	}
	rp, err := d.RiskParams(ctx, date)
	if err != nil {
		return fmt.Errorf("读取生效风控参数失败: %w", err)
	}
	// 收盘后先用当日收盘抬升持仓期间高点：这是移动止盈回撤基准的兜底更新
	// （盘中扫描每 5 分钟也抬一次；网络/行情缺失导致盘中没跑成时，这里补上）。
	if err := raiseHighWatermarks(ctx, d, date); err != nil {
		return fmt.Errorf("更新持仓期间高点: %w", err)
	}
	caps := WeakCapsOf(d.Config)
	cands, budget, rp, err := screenCandidates(ctx, rc, d, date, rp, caps)
	if err != nil {
		return fmt.Errorf("③ 选股: %w", err)
	}
	// 弱势试探留痕：门槛被人为关闭（gate_enabled=false）且大盘仍弱势时，风控已在
	// ScreenBudget 里按同一份 caps 收缩到试探档，漏斗与决策链共用同一条单票上限线
	// （不收紧的话，"放开闸门"与"满仓接飞刀"是同一件事）。这里只负责让判定可归因。
	if budget.WeakRegime {
		note := fmt.Sprintf("%s；试探风控收紧：总仓≤%.0f%% 单票≤%.0f%%",
			budget.WeakNote, caps.TotalPct*100, caps.SinglePct*100)
		rc.Degrade("WEAK_REGIME", note)
		d.raiseWeakTrace(ctx, date, note)
		observability.S().Infow("弱势试探模式", "date", date, "note", note)
	}
	// ETF 补口（弱势试探期专属）：科技类个股一手就超过试探单票上限，光靠个股漏斗
	// 在弱势期选不出任何"买得起"的科技敞口。白名单 → 流动性两级，不排名，
	// 与个股候选并入同一批交给同一个 LLM 评审。读不到数只降级，不断链。
	cands = appendETFCandidates(ctx, rc, d, date, cands, budget)
	if err := buildTickets(ctx, rc, d, date, cands, rp); err != nil {
		return fmt.Errorf("④ 买卖决策: %w", err)
	}
	return nil
}

// raiseHighWatermarks 用当日收盘价抬升每个持仓的期间最高价（只升不降）。
//
// 移动止盈的回撤基准就是这个列，而它此前只在买入成交时写入 —— 不补这一手，
// "自高点回撤"永远从买入价起算，移动止盈形同虚设。
func raiseHighWatermarks(ctx context.Context, d Deps, date string) error {
	pos, err := d.Store.TradeRepo().ListPositions(ctx)
	if err != nil {
		return fmt.Errorf("读取持仓失败: %w", err)
	}
	for _, p := range pos {
		if p.TotalQty <= 0 {
			continue
		}
		bar, err := d.Store.ScreenRepo().LatestBarAt(ctx, p.TsCode, date)
		if err != nil {
			return fmt.Errorf("读取持仓 %s 收盘价失败: %w", p.TsCode, err)
		}
		if err := d.Store.TradeRepo().RaiseHighPrice(ctx, p.TsCode, bar.RawClose); err != nil {
			return err
		}
	}
	return nil
}

// syncTodayBars 刷新在市股票清单 + 拉取当日行情，并把选股窗口内的缺口一并补齐。
//
// 回补天数由选股器给出（它是最深消费者）：以前固定回补 10 天而因子窗口要 20 根，
// 新库永远凑不满窗口。每补一天 = 每个接口一次调用（返回全市场），不是逐只调用。
func syncTodayBars(ctx context.Context, rc *observability.RunCtx, d Deps, date string) error {
	// 日历必须在日线之前：SyncDaily 靠日历挑"回补哪几天"，空日历会让它算出 0 个日期，
	// 于是一行日线都不写却照样返回成功。
	if err := renewCalendar(ctx, d, date); err != nil {
		return fmt.Errorf("交易日历补齐失败: %w", err)
	}
	if err := d.Dataloader.SyncStockBasics(ctx); err != nil {
		return fmt.Errorf("股票清单同步失败: %w", err)
	}
	rc.Declare("rows", "daily_bar", -1)
	if err := d.Dataloader.SyncDaily(ctx, date, d.Screener.SyncBackDays()); err != nil {
		return err
	}
	// ETF 补口挂在个股日线之后、同一交易日、同一条幂等写入路径：开不开由总闸决定，
	// 关掉时这一段一行都不执行（部署当日零行为变化）。
	d.syncETFBars(ctx, rc, date)
	n, err := d.Store.MarketRepo().CountBar(ctx, date)
	if err != nil {
		return fmt.Errorf("核对日线行数失败: %w", err)
	}
	rc.Actual("daily_bar", n)
	return nil
}

// syncETFBars ETF 补口的当日同步（screen.etf_enabled=false 时整段不执行）。
//
// 刻意"只降级不中止"：这条链是为了补"弱势期个股链选不出可负担科技标的"这个缺口，
// 不是主链。fund_daily/fund_adj 某天没发布或被限流时，正确反应是当日少一个候选来源
// （选股侧会因"窗口日线不足/当日无报价"把它剔掉并把原因写进 Notes），
// 而不是把整条 evening_pipeline 一起掐掉——个股候选不该为 ETF 的可用性陪葬。
func (d Deps) syncETFBars(ctx context.Context, rc *observability.RunCtx, date string) {
	raw := d.Config.GetString("screen.etf_enabled")
	enabled, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil || !enabled {
		return // 非法值在装配期已被 validateETF 拦下；这里兜的是键缺失的旧库
	}
	codes := etfCodesOf(d.Config)
	if len(codes) == 0 {
		d.raiseW(rc, "ETF_POOL_EMPTY", "ETF 已启用但白名单解析不出代码", "检查 screen.etf_whitelist")
		return
	}
	rc.Declare("rows", "etf_bar", 0)
	n, err := d.Dataloader.SyncETF(ctx, date, codes)
	rc.Actual("etf_bar", n)
	if err != nil {
		d.raiseW(rc, "ETF_SYNC", "ETF 日线同步降级（不中止当日链路）", err.Error())
		return
	}
	observability.S().Infow("ETF 日线同步完成", "date", date, "codes", len(codes), "rows", n)
}

// etfCodesOf 从 ETF 白名单取代码列（同步侧只需要代码）。
//
// 与 MarketGateOf 同一条兜底口径：装配期的 validateETF 已经把非法值拦在启动之外，
// 这里的宽容解析只为"键缺失的旧库"和"运行期被人改坏"这两条路径留出降级空间。
//
// 解析本身收拢到 screener 那一份（ETFWhitelistCodes）：分隔符集合、code:name:track
// 三段式、裸六位代码补交易所后缀、大小写与去重都在那里。同步侧与漏斗侧必须读到
// 同一串代码 —— 两处各自 split 迟早漂移，而漂移的症状是"日线明明同步了却整天
// 窗口日线不足"，是最难归因的一类。依赖方向是 app/scheduler → screener，不构成环。
func etfCodesOf(cfg ConfigReader) []string {
	return screener.ETFWhitelistCodes(cfg.GetString("screen.etf_whitelist"))
}

// gateFreshness 数据新鲜度门禁。不新鲜返回 error → 整链中止，当日不出任何指令。
//
// 这是"下游不得跑在上游之前"的唯一收口点：行情没出全时以前会让选股在旧数据上
// degraded 定稿，而调度器把 degraded 视作当日已完成，后续不再重跑。
func gateFreshness(ctx context.Context, d Deps, date string) error {
	rep, err := d.Freshness.Check(ctx, date)
	if err != nil {
		return fmt.Errorf("门禁执行失败: %w", err)
	}
	if !rep.Fresh {
		return fmt.Errorf("数据不新鲜: %s", rep.String())
	}
	return nil
}

// screenCandidates 跑选股漏斗（全程内存），返回候选、本轮大盘口径与**生效后的**风控参数
// （弱势收缩发生在 ScreenBudget 内，调用方拿到的 rp 与漏斗用的是同一份）。
func screenCandidates(ctx context.Context, rc *observability.RunCtx, d Deps, date string,
	rp risk.RiskParams, caps WeakCaps) ([]model.Candidate, screener.Budget, risk.RiskParams, error) {
	budget, rp, err := ScreenBudget(ctx, d.Store, d.Ledger, date, rp, MarketGateOf(d.Config), caps)
	if err != nil {
		return nil, budget, rp, err
	}
	rep, err := d.Screener.Run(ctx, date, budget)
	if err != nil {
		return nil, budget, rp, err
	}
	rc.Declare("rows", "candidates", candidateExpect(rep))
	rc.Actual("candidates", len(rep.Candidates))
	// 每一级漏斗的存量都写进运行日志：期望 0，因此不会因"筛空"被判缺失。
	for _, st := range rep.Stages {
		key := "screen_" + st.Slug
		rc.Declare("rows", key, 0)
		rc.Actual(key, st.Out)
	}
	if rep.Empty {
		rc.Degrade("SCREEN_EMPTY", emptyReason(rep))
	}
	return rep.Candidates, budget, rp, nil
}

// candidateExpect 候选产出物的期望数量。
//
// 关闸当日候选必为 0，那是规则的结论而不是缺产出——按 -1 断言会让每个关闸日稳定
// 产出一条 ARTIFACT_MISSING，紧跟在同一句"（非故障）"后面进同一封邮件。
// 开闸时仍要求至少 1：漏斗正常执行却一只都没选出来，才是要被拦住的情况。
func candidateExpect(rep *screener.Report) int {
	if rep.RegimeClosed {
		return 0
	}
	return -1
}

// emptyReason 候选为空的人话原因：区分"大盘闸门关闭"（设计内、非故障）与
// "漏斗某级把票筛光"。日报/计划邮件直接引用这句，用户不必去翻日志。
func emptyReason(rep *screener.Report) string {
	if rep.RegimeClosed {
		return "大盘在 " + rep.RegimeMALabel() + " 下方，当日按规则关闭买入漏斗（非故障）；" + rep.ShadowBrief()
	}
	// 非关闸：报出最后一级把池子筛到 0 的环节。
	var last string
	for _, st := range rep.Stages {
		if st.Out == 0 {
			last = st.Name
			break
		}
	}
	if last != "" {
		return "漏斗在「" + last + "」筛光候选（打分样本 " + itoa(rep.ScoredTotal) + " 只）"
	}
	return "候选 0 条（打分样本 " + itoa(rep.ScoredTotal) + " 只）"
}

// itoa 小整型转串（避免为一处拼接引入 strconv 依赖）。
func itoa(n int) string { return fmt.Sprintf("%d", n) }

// ScreenBudget 组装选股漏斗的资金与大盘口径：
// 单笔预算 = 可用现金 / 计划持仓数，再收口到风控单票上限；门槛开启且大盘跌破均线时当日关闭买入漏斗。
//
// 返回第二个值是**本轮生效的风控参数**：弱势判定要收缩风控，而漏斗必须和决策链用同一条
// 单票上限线，所以收缩在这里一次完成、把结果交回调用方，而不是"漏斗用旧口径选完、
// 决策链再按新口径把高价股全部否决"（那正是 20260930/20261008 的实跑形态）。
//
// 现金是这道闸门必需 inputs：拿不到就失败——那等于在最该保守的时候默认放行买入。
// 指数口径则随 gate 分两种：gate.Enabled 时读不到/算不出均线同样失败（历史行为，
// 拿不到基准就把"大盘恶化"判成"没恶化"是静默放行）；gate 关闭时指数只参与弱势
// 判定（detectWeakRegime，best-effort 永不失败），不再把数据问题升级成整链停摆——
// 那正是这次要修的故障模式。
//
// 调度器与 `jingzhe run task screen` 共用这一个实现：两边各写一套判据，
// 手工复现出的漏斗就与到点自动跑的不一致（历史上 CLI 那套把 MarketOK 写死为 true）。
func ScreenBudget(ctx context.Context, st *store.Store, led *ticket.Ledger,
	date string, rp risk.RiskParams, gate store.MarketGate, caps WeakCaps) (screener.Budget, risk.RiskParams, error) {
	b := screener.Budget{Slots: rp.MaxPositions, MAWindow: gate.Window}
	ast, err := led.Assets(ctx, date)
	if err != nil {
		return b, rp, fmt.Errorf("读取账户现金失败，可用资金筛无法判定: %w", err)
	}
	b.Cash = ast.Cash
	if !gate.Enabled {
		b.MarketOK = true
		// 门槛被人为关闭不等于大盘变好：弱势判定照常做，只是不再据此关闸，
		// 而是收缩到试探风控后让漏斗与决策链一起用它。指数读不到也算弱势——数据未知时宁缩量不满仓。
		b.WeakRegime, b.WeakNote = detectWeakRegime(ctx, st, date, gate.Window)
		if b.WeakRegime {
			rp = risk.WeakParams(rp, caps.TotalPct, caps.SinglePct)
		}
	} else {
		idx, err := st.ScreenRepo().LatestMarketIndex(ctx, date, gate.Window)
		if err != nil {
			return b, rp, err
		}
		if idx.MA <= 0 {
			return b, rp, fmt.Errorf("大盘指数 %s 在 %s 前不足 %d 根日线，MA%d 不可算",
				store.MarketIndex, date, gate.Window, gate.Window)
		}
		b.MarketOK = idx.Close >= idx.MA
	}
	// 单票上限按生效后的 rp 算：弱势收缩在上一步已经落进 rp。
	b.CapFen = rp.SingleCapFen()
	return b, rp, nil
}

// detectWeakRegime 门槛关闭时的大盘弱势判定（best-effort，永不返回 error）：
//
// 与闸门开启路径的判据相反是刻意的——那时"读不到指数"必须整链失败（拿不到基准
// 就把恶化判成没恶化是静默放行），而这里闸门已开、弱势只影响仓位收缩幅度，
// 数据不可用就按最保守的"弱势"处理并把这个原因写进 note 供归因。
func detectWeakRegime(ctx context.Context, st *store.Store, date string, window int) (bool, string) {
	if window < 1 || window > store.MarketMAWindow {
		window = store.MarketMAWindow
	}
	idx, err := st.ScreenRepo().LatestMarketIndex(ctx, date, window)
	if err != nil {
		return true, fmt.Sprintf("指数 %s 数据不可读（%v），按弱势降级", store.MarketIndex, err)
	}
	if idx.MA <= 0 {
		return true, fmt.Sprintf("指数 %s 在 %s 前不足 %d 根日线，均线不可算，按弱势降级", store.MarketIndex, date, window)
	}
	if idx.Close < idx.MA {
		return true, fmt.Sprintf("指数 %s 收盘在 MA%d 下方", store.MarketIndex, window)
	}
	return false, ""
}

// gateOffCapsOf 弱势试探的两个仓位上限（占总资产比例），读取口径与 MarketGateOf 一致：
// 键缺失或坏值回落到 risk 包的弱势默认档，越界值钳回物理熔断——本函数只可能比配置更严。
func gateOffCapsOf(cfg ConfigReader) (total, single float64) {
	total, err := strconv.ParseFloat(strings.TrimSpace(cfg.GetString("screen.gate_off_max_total_pct")), 64)
	if err != nil || total <= 0 {
		total = risk.WeakMaxTotalPctDefault
	}
	if total > risk.CircuitMaxTotalPct {
		total = risk.CircuitMaxTotalPct
	}
	single, err = strconv.ParseFloat(strings.TrimSpace(cfg.GetString("screen.gate_off_max_single_pct")), 64)
	if err != nil || single <= 0 {
		single = risk.WeakMaxSinglePctDefault
	}
	if single > risk.CircuitMaxSinglePct {
		single = risk.CircuitMaxSinglePct
	}
	return total, single
}

// WeakCaps 弱势试探的两个上限（占总资产比例），成对传递避免两个裸 float 参数搞混顺序。
type WeakCaps struct {
	TotalPct  float64
	SinglePct float64
}

// WeakCapsOf 读出弱势上限的结构化形态（判据全在 gateOffCapsOf 里，这里只做封装）。
func WeakCapsOf(cfg ConfigReader) WeakCaps {
	total, single := gateOffCapsOf(cfg)
	return WeakCaps{TotalPct: total, SinglePct: single}
}

// raiseWeakTrace 弱势判定单独落一条 alert:WEAK_REGIME 轨迹（与 alert:SCREEN_EMPTY
// 同口径的一行一事，工程 20260924 建议）：
//
// rc.Degrade 只把原因附在整条 job 行的 detail 里，试探模式跑上几周后想按日期捞
// "哪天判了弱势、为什么"，单独一行才可查。写失败不影响交易链路——诊断缺行
// 不能反过来把当日决策一起掐掉，但必须留日志，否则等于什么都没发生。
func (d Deps) raiseWeakTrace(ctx context.Context, date, detail string) {
	trace := model.RunTrace{
		TradeDate: date, Subject: model.TraceAlert("WEAK_REGIME"),
		Outcome: model.TracePartial,
		Detail:  date + " 门槛关闭且大盘弱势，进入小仓位试探模式：" + detail,
		At:      time.Now().UTC().Format(time.RFC3339),
	}
	if err := d.Store.TraceRepo().Write(ctx, trace); err != nil {
		observability.S().Errorw("写 WEAK_REGIME 轨迹失败", "date", date, "err", err.Error())
	}
}

// buildTickets 由候选与持仓生成买卖决策并写入待买卖表（drafted，等 17:00 发邮件）。
//
// 买入决策权在 LLM 评审员手里，因此 llm.enabled=false 不是"少一道终审"，而是"当日不可能有买单"——
// 这一点必须显式告警，否则人会以为流水线跑通了却什么都没买到。
func buildTickets(ctx context.Context, rc *observability.RunCtx, d Deps, date string,
	cands []model.Candidate, rp risk.RiskParams) error {
	if !d.Decider.Enabled() {
		d.raiseU(rc, "LLM_DISABLED", "买入决策未启用，当日不会有任何买单",
			"llm.enabled=false 或 api_key/model 缺失；风控参数不会替代决策者")
	}
	rep, err := d.Signal.Generate(ctx, date, cands, rp, d.Decider)
	if err != nil {
		return err
	}
	rc.Declare("rows", "pending_tickets", 0) // 期望 0：评审后决定都不买是正常结果，不是缺产出
	rc.Actual("pending_tickets", rep.Tickets)
	rc.Declare("rows", "llm_declined", 0) // 期望 0：评审否决是正常工作结果，不是缺失
	rc.Actual("llm_declined", rep.Declined)
	for _, n := range rep.Notes {
		observability.S().Infow("决策阶段提示", "date", date, "note", n)
	}
	observability.S().Infow("买卖决策完成", "date", date, "candidates", rep.Candidates,
		"approved", rep.Approved, "declined", rep.Declined, "review_failed", rep.Failed,
		"sell", rep.SellSignals, "rejected", rep.Rejected,
		"tickets", rep.Tickets, "skipped_existing", rep.Skipped)
	if rep.Failed > 0 {
		msg := fmt.Sprintf("%d 只候选评审未问出结果，明细见当日轨迹的 llm:* 失败行", rep.Failed)
		d.raiseU(rc, "LLM_FAILED", "买入评审部分失败，当日这些标的不建仓", msg)
		rc.Degrade("LLM_FAILED", msg)
	}
	if rep.Rejected > 0 && rep.Tickets == 0 && rep.Approved+rep.SellSignals > 0 {
		d.raiseU(rc, "ALL_REJECTED", "有决策但全被风控否决", fmt.Sprintf("否决 %d 条", rep.Rejected))
		rc.Degrade("ALL_REJECTED", fmt.Sprintf("否决 %d 条", rep.Rejected))
	}
	// 有候选却 0 指令：候选非空说明漏斗正常，是"评审全否/风控全拒"这类要交代的结论。
	// 单列一条降级让日报/计划邮件能引用（不重复：ALL_REJECTED 已覆盖"有决策被全拒"）。
	if len(cands) > 0 && rep.Tickets == 0 && rep.Rejected == 0 {
		rc.Degrade("NO_TICKET_FROM_CANDIDATES",
			fmt.Sprintf("有 %d 只候选但当日 0 指令（模型否决 %d、未问出 %d）",
				len(cands), rep.Declined, rep.Failed))
	}
	return nil
}
