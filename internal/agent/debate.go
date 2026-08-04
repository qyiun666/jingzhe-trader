package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

type DebateOrchestrator struct {
	llm        *llm.Client
	barRepo    *store.BarRepo
	basicRepo  *store.BasicRepo
	finaRepo   *store.FinaRepo
	newsRepo   *store.NewsRepo
	debateRepo *store.DebateRepo
	tech       *TechnicalAnalyst
	fund       *FundamentalAnalyst
	news       *NewsAnalyst
	market     *MarketAnalyst
	bull       *BullResearcher
	bear       *BearResearcher
	riskMgr    *RiskManagerAgent
}

func NewDebateOrchestrator(llmClient *llm.Client, barRepo *store.BarRepo, basicRepo *store.BasicRepo, finaRepo *store.FinaRepo, newsRepo *store.NewsRepo, debateRepo *store.DebateRepo) *DebateOrchestrator {
	o := &DebateOrchestrator{llm: llmClient, barRepo: barRepo, basicRepo: basicRepo, finaRepo: finaRepo, newsRepo: newsRepo, debateRepo: debateRepo}
	o.tech = NewTechnicalAnalyst(llmClient, barRepo)
	o.fund = NewFundamentalAnalyst(llmClient, basicRepo, finaRepo)
	o.news = NewNewsAnalyst(llmClient, newsRepo)
	o.market = NewMarketAnalyst(llmClient, barRepo)
	o.bull = NewBullResearcher(llmClient)
	o.bear = NewBearResearcher(llmClient)
	o.riskMgr = NewRiskManagerAgent(llmClient)
	return o
}

func (o *DebateOrchestrator) IsEnabled() bool {
	return o != nil && o.llm != nil && o.llm.IsEnabled()
}

func (o *DebateOrchestrator) Debate(ctx *DebateContext) (*DebateResult, error) {
	logger.L().Infof("[智能体辩论] 开始 %s (%s) %s", ctx.TsCode, ctx.Name, ctx.TradeDate)
	reports := o.runAnalystsParallel(ctx)
	bullArg, err := o.bull.Research(ctx, reports)
	if err != nil {
		logger.L().Warnw("看涨研究员失败", "ts_code", ctx.TsCode, "err", err)
		bullArg = &ResearchArgument{Side: "bull", Sentiment: 0, Confidence: 0.2}
	}
	bearArg, err := o.bear.Research(ctx, reports)
	if err != nil {
		logger.L().Warnw("看跌研究员失败", "ts_code", ctx.TsCode, "err", err)
		bearArg = &ResearchArgument{Side: "bear", Sentiment: 0, Confidence: 0.2}
	}
	result, err := o.riskMgr.Judge(ctx, reports, bullArg, bearArg)
	if err != nil || result == nil {
		logger.L().Warnw("风险管理员裁决失败, 使用降级逻辑", "ts_code", ctx.TsCode, "err", err)
		result = o.riskMgr.fallbackJudge(ctx, reports, bullArg, bearArg)
	}
	if result == nil {
		return nil, fmt.Errorf("辩论无结果: %s", ctx.TsCode)
	}
	if o.debateRepo != nil {
		if _, err := o.debateRepo.Insert(result); err != nil {
			logger.L().Warnw("辩论结果落库失败", "ts_code", ctx.TsCode, "err", err)
		}
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
				report = &AnalysisReport{Agent: analyst.Name(), TsCode: ctx.TsCode, Confidence: 0.1}
			}
			reports[idx] = report
		}(i, a)
	}
	wg.Wait()
	return reports
}

func (o *DebateOrchestrator) EnhanceSignals(date string, signals []model.Signal, bars map[string]*model.Bar, positions map[string]*model.Position, totalAsset float64, stockNames map[string]string) []model.Signal {
	if !o.IsEnabled() {
		return signals
	}
	marketBars := make(map[string]*model.Bar)
	for _, code := range []string{"000300.SH", "000001.SH", "399001.SZ"} {
		if bar, ok := bars[code]; ok {
			marketBars[code] = bar
		}
	}
	enhanced := make([]model.Signal, 0, len(signals))
	for _, sig := range signals {
		if sig.Direction == model.DirSell {
			enhanced = append(enhanced, sig)
			continue
		}
		ctx := o.buildContext(date, sig.TsCode, stockNames, bars, positions, totalAsset, marketBars)
		if ctx == nil {
			enhanced = append(enhanced, sig)
			continue
		}
		result, err := o.Debate(ctx)
		if err != nil || result == nil {
			enhanced = append(enhanced, sig)
			continue
		}
		switch result.Decision {
		case "reject", "hold", "sell":
			// reject=否决买入, hold=建议观望, sell=建议卖出(对买入信号而言都意味着不买入)
			logger.L().Infof("[智能体辩论] %s 信号被过滤 %s: decision=%s summary=%s",
				result.Decision, sig.TsCode, result.Decision, result.Summary)
			continue
		case "buy":
			// 按辩论建议的仓位比例调整数量 (PositionPct 0~0.6, 不超过原始目标)
			if result.PositionPct > 0 && result.PositionPct < 1 {
				adjustedQty := int(float64(sig.TargetQty) * result.PositionPct)
				if adjustedQty > 0 && adjustedQty <= sig.TargetQty {
					sig.TargetQty = adjustedQty
				}
			}
			sig.Reason = fmt.Sprintf("%s | LLM辩论: %s", sig.Reason, result.Summary)
			sig.Strength = result.Confidence
		}
		enhanced = append(enhanced, sig)
	}
	return enhanced
}

func (o *DebateOrchestrator) buildContext(date, tsCode string, stockNames map[string]string, bars map[string]*model.Bar, positions map[string]*model.Position, totalAsset float64, marketBars map[string]*model.Bar) *DebateContext {
	name := tsCode
	if n, ok := stockNames[tsCode]; ok {
		name = n
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
	return &DebateContext{TradeDate: date, TsCode: tsCode, Name: name, Bars: history, Position: pos, TotalAsset: totalAsset, MarketBars: marketBars}
}

func dateMinusDays(date string, days int) string {
	t, err := time.Parse("20060102", date)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, -days).Format("20060102")
}

func ParseDebateArgs(s string) []string {
	if s == "" {
		return nil
	}
	var args []string
	json.Unmarshal([]byte(s), &args)
	return args
}
