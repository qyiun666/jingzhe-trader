package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// DebateSettings 辩论编排器的运行参数, 由组合根从 config 映射注入
// (agent 不直接依赖 config 包, 保持能力包身份中立)
type DebateSettings struct {
	Positions     PositionLimits // 风控仓位约束, 用于生成提示词与换算建议仓位
	MarketIndexes []string       // 参与大盘判断的指数代码 (来自 dataloader.watchlist)
}

type DebateOrchestrator struct {
	llm           *llm.Client
	barRepo       *store.BarRepo
	basicRepo     *store.BasicRepo
	finaRepo      *store.FinaRepo
	newsRepo      *store.NewsRepo
	stockRepo     *store.StockRepo
	debateRepo    *store.DebateRepo
	reviewRepo    *store.DebateReviewRepo
	moneyflowRepo *store.MoneyFlowRepo
	toplistRepo   *store.TopListRepo
	settings      DebateSettings
	tech          *TechnicalAnalyst
	fund          *FundamentalAnalyst
	news          *NewsAnalyst
	market        *MarketAnalyst
	bull          *researcher
	bear          *researcher
	riskMgr       *RiskManagerAgent
}

// DebateDeps 辩论编排器依赖, 由组合根一次性装配
type DebateDeps struct {
	LLM           *llm.Client
	BarRepo       *store.BarRepo
	BasicRepo     *store.BasicRepo
	FinaRepo      *store.FinaRepo
	NewsRepo      *store.NewsRepo
	StockRepo     *store.StockRepo // 所属行业 (消息面在本数据档位下只能按行业检索)
	DebateRepo    *store.DebateRepo
	ReviewRepo    *store.DebateReviewRepo
	MoneyFlowRepo *store.MoneyFlowRepo
	TopListRepo   *store.TopListRepo
	Settings      DebateSettings
}

func NewDebateOrchestrator(d DebateDeps) *DebateOrchestrator {
	o := &DebateOrchestrator{llm: d.LLM, barRepo: d.BarRepo, basicRepo: d.BasicRepo, finaRepo: d.FinaRepo,
		newsRepo: d.NewsRepo, stockRepo: d.StockRepo, debateRepo: d.DebateRepo, reviewRepo: d.ReviewRepo,
		moneyflowRepo: d.MoneyFlowRepo, toplistRepo: d.TopListRepo, settings: d.Settings}
	o.tech = NewTechnicalAnalyst(o.llm, o.barRepo)
	o.fund = NewFundamentalAnalyst(o.llm, o.basicRepo, o.finaRepo)
	o.news = NewNewsAnalyst(o.llm, o.newsRepo)
	o.market = NewMarketAnalyst(o.llm, o.barRepo, o.settings.MarketIndexes)
	o.bull = NewResearcher(o.llm, "bull")
	o.bear = NewResearcher(o.llm, "bear")
	o.riskMgr = NewRiskManagerAgent(o.llm, o.settings.Positions)
	return o
}

func (o *DebateOrchestrator) IsEnabled() bool {
	return o != nil && o.llm != nil && o.llm.IsEnabled()
}

// Debate 组织一轮完整辩论 (分析师并行 → 多空研究员 → 风险管理员裁决), 返回最终结论
// 结论落库由调用方负责 (见 EnhanceSignals): 结论是 7 次 LLM 调用换来的, 不能因为存不下就丢弃,
// 但落库失败必须可上报 —— 此前只记一条 Warn 便返回成功, 导致辩论表长期空表且无人察觉
func (o *DebateOrchestrator) Debate(ctx *DebateContext) (*DebateResult, error) {
	logger.L().Infof("[智能体辩论] 开始 %s (%s) %s", ctx.TsCode, ctx.Name, ctx.TradeDate)
	reports := o.runAnalystsParallel(ctx)
	bullArg, bearArg := o.runResearchersParallel(ctx, reports)
	result, err := o.riskMgr.Judge(ctx, reports, bullArg, bearArg)
	if err != nil || result == nil {
		logger.L().Warnw("风险管理员裁决失败, 使用降级逻辑", "ts_code", ctx.TsCode, "err", err)
		result = o.riskMgr.fallbackJudge(ctx, reports, bullArg, bearArg)
	}
	if result == nil {
		return nil, fmt.Errorf("辩论无结果: %s", ctx.TsCode)
	}
	logger.L().Infof("[智能体辩论] 完成 %s: decision=%s confidence=%.2f", ctx.TsCode, result.Decision, result.Confidence)
	return result, nil
}

func (o *DebateOrchestrator) runAnalystsParallel(ctx *DebateContext) []*AnalysisReport {
	analysts := []Analyst{o.tech, o.fund, o.news, o.market}
	reports := make([]*AnalysisReport, len(analysts))
	var wg sync.WaitGroup
	for i, a := range analysts {
		wg.Add(1)
		go func(idx int, analyst Analyst) {
			defer wg.Done()
			report, err := analyst.Analyze(ctx)
			if err != nil {
				logger.L().Warnw("分析师执行失败", "agent", analyst.Name(), "ts_code", ctx.TsCode, "err", err)
				// 打缺失标记而非 Confidence=0.1: 后者无法与模型真实给出的低置信度区分,
				// 会把"没有意见"当成一条偏空意见计入风控平均 sentiment
				report = &AnalysisReport{
					Agent:     analyst.Name(),
					TsCode:    ctx.TsCode,
					KeyPoints: []string{reportMissingData},
				}
			}
			reports[idx] = report
		}(i, a)
	}
	wg.Wait()
	return reports
}

// runResearchersParallel 并行执行看涨/看跌研究员 (失败时降级为空论点, 与串行行为一致)
func (o *DebateOrchestrator) runResearchersParallel(ctx *DebateContext, reports []*AnalysisReport) (*ResearchArgument, *ResearchArgument) {
	type researchResult struct {
		arg *ResearchArgument
		err error
	}
	bullCh := make(chan researchResult, 1)
	bearCh := make(chan researchResult, 1)
	go func() {
		arg, err := o.bull.Research(ctx, reports)
		bullCh <- researchResult{arg: arg, err: err}
	}()
	go func() {
		arg, err := o.bear.Research(ctx, reports)
		bearCh <- researchResult{arg: arg, err: err}
	}()
	bullRes := <-bullCh
	bearRes := <-bearCh

	bullArg := bullRes.arg
	if bullRes.err != nil {
		logger.L().Warnw("看涨研究员失败", "ts_code", ctx.TsCode, "err", bullRes.err)
		bullArg = missingArgument("bull")
	}
	bearArg := bearRes.arg
	if bearRes.err != nil {
		logger.L().Warnw("看跌研究员失败", "ts_code", ctx.TsCode, "err", bearRes.err)
		bearArg = missingArgument("bear")
	}
	return bullArg, bearArg
}

// missingArgument 研究员调用失败时的占位论点
// Confidence 取 0 而非 0.1: 后者与模型真实给出的低置信度无法区分, 会被风控当成一条有效意见
func missingArgument(side string) *ResearchArgument {
	return &ResearchArgument{Side: side, Sentiment: 0, Confidence: 0, Arguments: []string{reportMissingData}}
}

// debateConcurrency 同时辩论的股票数上限 (LLM 全局限流在 client 层统一控制, 此上限防资源峰值)
const debateConcurrency = 4

// debateAdjustedQty 按 LLM 建议的仓位占比 (占总资产比例) 换算买入数量
// 返回 min(计划量, 建议量): 计划量是策略结合风控与资金算出的上界, 辩论只能在其之下缩量。
// 价格缺失或建议非正时原样返回, 不让换算失败反过来改变仓位。
func debateAdjustedQty(plannedQty int, totalAsset float64, bar *model.Bar, suggestedPct, maxPositionPct float64) int {
	if plannedQty <= 0 || bar == nil || bar.Close <= 0 || suggestedPct <= 0 {
		return plannedQty
	}
	if maxPositionPct > 0 && suggestedPct > maxPositionPct {
		suggestedPct = maxPositionPct // 提示词已声明该上限, 越界只可能是模型没守格式
	}
	qty := strategy.CalcBuyQty(totalAsset, bar.Close, suggestedPct)
	if qty <= 0 || qty > plannedQty {
		return plannedQty
	}
	return qty
}

// EnhanceSignals 对买入信号逐个跑辩论并按结论调整信号
// 第二个返回值为"结论已产出但落库失败"的清单 (ts_code: 原因): 信号照常增强,
// 但缺了库内记录就等于反思闭环 (ReviewDebates 回填命中率) 拿不到样本, 必须由调用方告警
func (o *DebateOrchestrator) EnhanceSignals(date string, signals []model.Signal, bars map[string]*model.Bar, positions map[string]*model.Position, totalAsset float64, stockNames map[string]string) ([]model.Signal, []string) {
	if !o.IsEnabled() {
		return signals, nil
	}

	// 股票间并行辩论: 每只股票独立 (LLM 调用已在 client 层限流), 按下标收集保证信号顺序稳定
	enhanced := make([]model.Signal, len(signals))
	kept := make([]bool, len(signals))

	var wg sync.WaitGroup
	var persistMu sync.Mutex
	var persistFailures []string
	sem := make(chan struct{}, debateConcurrency)
	for i, sig := range signals {
		if sig.Direction == model.DirSell {
			enhanced[i] = sig
			kept[i] = true
			continue
		}
		wg.Add(1)
		go func(idx int, sig model.Signal) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx := o.buildContext(date, sig.TsCode, stockNames, bars, positions, totalAsset)
			if ctx == nil {
				enhanced[idx] = sig
				kept[idx] = true
				return
			}
			result, err := o.Debate(ctx)
			if err != nil || result == nil {
				enhanced[idx] = sig
				kept[idx] = true
				return
			}
			// 落库失败不回滚本次增强, 只登记待告警
			if o.debateRepo != nil {
				if _, err := o.debateRepo.Insert(result); err != nil {
					logger.L().Errorf("[智能体辩论] %s 结论落库失败: %v", sig.TsCode, err)
					persistMu.Lock()
					persistFailures = append(persistFailures, fmt.Sprintf("%s: %v", sig.TsCode, err))
					persistMu.Unlock()
				}
			}
			switch result.Decision {
			case "reject", "hold", "sell":
				// reject=否决买入, hold=建议观望, sell=建议卖出(对买入信号而言都意味着不买入)
				logger.L().Infof("[智能体辩论] %s 信号被过滤: decision=%s summary=%s",
					sig.TsCode, result.Decision, result.Summary)
				return // 过滤, kept 保持 false
			case "buy":
				// position_pct 口径 = 拟买入金额占总资产比例 (与提示词一致), 按策略下单同一公式换算成数量。
				// 只收紧不放宽: 计划量已由策略结合风控与资金算出, 辩论只被允许缩量;
				// 放大交给后续 CheckAndSortSignals 统一裁, 避免这里绕过风控
				sig.TargetQty = debateAdjustedQty(sig.TargetQty, totalAsset, bars[sig.TsCode], result.PositionPct, o.settings.Positions.MaxPositionPct)
				sig.Reason = fmt.Sprintf("%s | LLM辩论: %s", sig.Reason, result.Summary)
				sig.Reason = appendStopPriceReason(sig.Reason, result.StopPrice)
				sig.Strength = result.Confidence
				enhanced[idx] = sig
				kept[idx] = true
			default:
				// LLM 输出不可控, 未知决策与旧行为一致保留原信号 (不静默丢单)
				logger.L().Warnw("[智能体辩论] 未知决策, 保留原信号", "decision", result.Decision, "ts_code", sig.TsCode)
				enhanced[idx] = sig
				kept[idx] = true
			}
		}(i, sig)
	}
	wg.Wait()

	out := make([]model.Signal, 0, len(signals))
	for i := range signals {
		if kept[i] {
			out = append(out, enhanced[i])
		}
	}
	return out, persistFailures
}

func (o *DebateOrchestrator) buildContext(date, tsCode string, stockNames map[string]string, bars map[string]*model.Bar, positions map[string]*model.Position, totalAsset float64) *DebateContext {
	name := tsCode
	if n, ok := stockNames[tsCode]; ok {
		name = n
	}
	industry := ""
	if o.stockRepo != nil {
		st, err := o.stockRepo.GetByCode(tsCode)
		if err != nil {
			logger.L().Warnw("查询所属行业失败, 消息面将只按名称/代码检索", "ts_code", tsCode, "err", err)
		} else if st != nil {
			industry = st.Industry
		}
	}
	startDate := dateMinusDays(date, 90)
	history, err := o.barRepo.GetBars(tsCode, startDate, date)
	if err != nil || len(history) < 10 {
		logger.L().Warnw("获取历史K线不足", "ts_code", tsCode, "len", len(history))
		return nil
	}
	var pos *model.Position
	if p, ok := positions[tsCode]; ok {
		pos = p
	}
	// 资金面数据 (近2周): 数据缺失不影响辩论, 仅上下文变薄
	var flows []model.MoneyFlow
	if o.moneyflowRepo != nil {
		if f, err := o.moneyflowRepo.GetByCode(tsCode, dateMinusDays(date, 14), date); err == nil {
			flows = f
		}
	}
	var tops []model.TopList
	if o.toplistRepo != nil {
		if t, err := o.toplistRepo.GetByCode(tsCode, dateMinusDays(date, 14), date); err == nil {
			tops = t
		}
	}
	// 历史辩论复盘 (反思闭环: 最近5次有方向决策的实际结果)
	var reviewSummary string
	if o.reviewRepo != nil {
		if reviews, err := o.reviewRepo.GetRecentByCode(tsCode, 5); err == nil && len(reviews) > 0 {
			reviewSummary = formatReviews(reviews, 5)
		}
	}
	return &DebateContext{TradeDate: date, TsCode: tsCode, Name: name, Industry: industry, Bars: history,
		Position: pos, TotalAsset: totalAsset,
		MoneyFlows: flows, TopLists: tops, ReviewSummary: reviewSummary}
}

// appendStopPriceReason 将风险管理员裁决的止损价附到买入信号原因中, 便于用户参考
func appendStopPriceReason(reason string, stopPrice float64) string {
	if stopPrice <= 0 {
		return reason
	}
	return fmt.Sprintf("%s, 建议止损价%.2f", reason, stopPrice)
}

func dateMinusDays(date string, days int) string {
	t, err := time.Parse("20060102", date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, -days).Format("20060102")
}

// dashedDate 把内部 YYYYMMDD 日期转成 news.datetime 使用的 "2006-01-02" 前缀形式
// 两种格式并存是历史原因 (行情表按交易日、新闻表按带时区的发布时间字符串), 转换只在此处发生一次
func dashedDate(date string) string {
	t, err := time.Parse("20060102", date)
	if err != nil {
		return date
	}
	return t.Format("2006-01-02")
}

func ParseDebateArgs(s string) []string {
	if s == "" {
		return nil
	}
	var args []string
	json.Unmarshal([]byte(s), &args)
	return args
}
