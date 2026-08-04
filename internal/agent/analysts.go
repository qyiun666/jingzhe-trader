package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

type TechnicalAnalyst struct {
	llm  *llm.Client
	repo *store.BarRepo
}

func NewTechnicalAnalyst(llmClient *llm.Client, repo *store.BarRepo) *TechnicalAnalyst {
	return &TechnicalAnalyst{llm: llmClient, repo: repo}
}
func (a *TechnicalAnalyst) Name() string { return "technical" }

func (a *TechnicalAnalyst) Analyze(ctx *DebateContext) (*AnalysisReport, error) {
	bars := ctx.Bars
	if len(bars) < 10 {
		return &AnalysisReport{Agent: a.Name(), TsCode: ctx.TsCode, Confidence: 0.1}, nil
	}
	last := bars[len(bars)-1]
	closes := extractCloses(bars)
	ma5 := sma(closes, 5)
	ma20 := sma(closes, 20)
	volRatio := 0.0
	if len(bars) > 6 && avgVol(bars[len(bars)-6:len(bars)-1], 5) > 0 {
		volRatio = last.Vol / avgVol(bars[len(bars)-6:len(bars)-1], 5)
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
近5日K线:
%s
最新收盘: %.2f  前收: %.2f  涨跌幅: %.2f%%
MA5: %.2f  MA20: %.2f  (MA5%sMA20)
成交量比(5日均量): %.2f
总资产: %.0f  持仓: %s

请从技术面分析该股票，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		formatRecentBars(bars, 5),
		last.Close, last.PreClose, last.PctChg,
		ma5, ma20, crossStr(ma5, ma20),
		volRatio,
		ctx.TotalAsset, posStr(ctx.Position))
	return callLLMCommon(a.llm, ctx.TsCode, "technical", technicalSysPrompt, userPrompt)
}

type FundamentalAnalyst struct {
	llm       *llm.Client
	basicRepo *store.BasicRepo
	finaRepo  *store.FinaRepo
}

func NewFundamentalAnalyst(llmClient *llm.Client, basicRepo *store.BasicRepo, finaRepo *store.FinaRepo) *FundamentalAnalyst {
	return &FundamentalAnalyst{llm: llmClient, basicRepo: basicRepo, finaRepo: finaRepo}
}
func (a *FundamentalAnalyst) Name() string { return "fundamental" }

func (a *FundamentalAnalyst) Analyze(ctx *DebateContext) (*AnalysisReport, error) {
	basics, err := a.basicRepo.GetByCode(ctx.TsCode, ctx.TradeDate, ctx.TradeDate)
	var basic *model.DailyBasic
	if err == nil && len(basics) > 0 {
		basic = &basics[0]
	}
	finas, _ := a.finaRepo.GetByCode(ctx.TsCode)
	var fina *model.FinaIndicator
	if len(finas) > 0 {
		fina = &finas[0]
	}
	if basic == nil && fina == nil {
		return &AnalysisReport{Agent: a.Name(), TsCode: ctx.TsCode, Confidence: 0.2, KeyPoints: []string{"无基本面数据"}}, nil
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
基本面: PE=%.1f PE_TTM=%.1f PB=%.2f 换手率=%.2f%% 量比=%.2f 市值=%.0f万
财务: EPS=%.2f ROE=%.2f%% 毛利率=%.1f%% 净利率=%.1f%% 资产负债率=%.1f%% 营收增速=%.1f%%

请从基本面分析该股票投资价值，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		safeFloat(basic, "PE"), safeFloat(basic, "PE_TTM"), safeFloat(basic, "PB"),
		safeFloat(basic, "TurnoverRate"), safeFloat(basic, "VolumeRatio"), safeFloat(basic, "TotalMV"),
		safeFinaFloat(fina, "EPS"), safeFinaFloat(fina, "ROE"),
		safeFinaFloat(fina, "GrossProfitMargin"), safeFinaFloat(fina, "NetProfitMargin"),
		safeFinaFloat(fina, "DebtToAssets"), safeFinaFloat(fina, "NetProfitYoy"))
	return callLLMCommon(a.llm, ctx.TsCode, "fundamental", fundamentalSysPrompt, userPrompt)
}

type NewsAnalyst struct {
	llm      *llm.Client
	newsRepo *store.NewsRepo
}

func NewNewsAnalyst(llmClient *llm.Client, newsRepo *store.NewsRepo) *NewsAnalyst {
	return &NewsAnalyst{llm: llmClient, newsRepo: newsRepo}
}
func (a *NewsAnalyst) Name() string { return "news" }

func (a *NewsAnalyst) Analyze(ctx *DebateContext) (*AnalysisReport, error) {
	news, err := a.newsRepo.GetRecent(20)
	if err != nil || len(news) == 0 {
		return &AnalysisReport{Agent: a.Name(), TsCode: ctx.TsCode, Confidence: 0.2, KeyPoints: []string{"无近期新闻"}}, nil
	}
	relevant := filterRelevantNews(news, ctx.Name, ctx.TsCode)
	if len(relevant) == 0 {
		relevant = news[:min(5, len(news))]
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
近期新闻:
%s

请分析新闻对%s的影响，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, formatNews(relevant), ctx.Name)
	return callLLMCommon(a.llm, ctx.TsCode, "news", newsSysPrompt, userPrompt)
}

type MarketAnalyst struct {
	llm  *llm.Client
	repo *store.BarRepo
}

func NewMarketAnalyst(llmClient *llm.Client, repo *store.BarRepo) *MarketAnalyst {
	return &MarketAnalyst{llm: llmClient, repo: repo}
}
func (a *MarketAnalyst) Name() string { return "market" }

func (a *MarketAnalyst) Analyze(ctx *DebateContext) (*AnalysisReport, error) {
	var indexText strings.Builder
	for _, code := range []string{"000300.SH", "000001.SH", "399001.SZ"} {
		if bar, ok := ctx.MarketBars[code]; ok {
			indexText.WriteString(fmt.Sprintf("%s: 收盘%.2f 涨跌%.2f%%  |  ", code, bar.Close, bar.PctChg))
		}
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
大盘指数: %s
该股当日涨跌: %.2f%%

请分析当前市场环境对该股的影响，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, indexText.String(), safeBarPctChg(ctx.Bars))
	return callLLMCommon(a.llm, ctx.TsCode, "market", marketSysPrompt, userPrompt)
}

func callLLMCommon(client *llm.Client, tsCode, agentType, sysPrompt, userPrompt string) (*AnalysisReport, error) {
	if client == nil || !client.IsEnabled() {
		return &AnalysisReport{Agent: agentType, TsCode: tsCode, Confidence: 0.3, KeyPoints: []string{"LLM未启用"}}, nil
	}
	resp, err := client.Chat(sysPrompt, userPrompt)
	if err != nil {
		logger.L().Warnw("分析师LLM调用失败", "agent", agentType, "ts_code", tsCode, "err", err)
		return &AnalysisReport{Agent: agentType, TsCode: tsCode, Confidence: 0.2, KeyPoints: []string{"LLM调用失败"}}, nil
	}
	resp = stripJSON(resp)
	var report AnalysisReport
	report.Agent = agentType
	report.TsCode = tsCode
	if err := json.Unmarshal([]byte(resp), &report); err != nil {
		logger.L().Warnw("分析师响应解析失败", "agent", agentType, "ts_code", tsCode, "raw", resp[:min(200, len(resp))])
		return &AnalysisReport{Agent: agentType, TsCode: tsCode, Confidence: 0.2, KeyPoints: []string{"响应解析失败"}}, nil
	}
	return &report, nil
}

func stripJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func extractCloses(bars []model.Bar) []float64 {
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	return closes
}

func sma(values []float64, period int) float64 {
	if len(values) < period {
		return 0
	}
	sum := 0.0
	for i := len(values) - period; i < len(values); i++ {
		sum += values[i]
	}
	return sum / float64(period)
}

func avgVol(bars []model.Bar, n int) float64 {
	if len(bars) == 0 {
		return 1
	}
	sum := 0.0
	for _, b := range bars {
		sum += b.Vol
	}
	return sum / float64(len(bars))
}

func formatRecentBars(bars []model.Bar, n int) string {
	start := 0
	if len(bars) > n {
		start = len(bars) - n
	}
	var sb strings.Builder
	for i := start; i < len(bars); i++ {
		b := bars[i]
		sb.WriteString(fmt.Sprintf("  %s O=%.2f H=%.2f L=%.2f C=%.2f V=%.0f\n", b.TradeDate, b.Open, b.High, b.Low, b.Close, b.Vol))
	}
	return sb.String()
}

func crossStr(short, long float64) string {
	if short > long {
		return "上穿(多头)"
	}
	return "下穿(空头)"
}

func posStr(p *model.Position) string {
	if p == nil || p.TotalQty == 0 {
		return "无持仓"
	}
	return fmt.Sprintf("%d股 成本%.2f 浮动%.2f%%", p.TotalQty, p.CostPrice, p.FloatingPnLPct*100)
}

func safeFloat(b *model.DailyBasic, field string) float64 {
	if b == nil {
		return 0
	}
	switch field {
	case "PE":
		return b.PE
	case "PE_TTM":
		return b.PE_TTM
	case "PB":
		return b.PB
	case "TurnoverRate":
		return b.TurnoverRate
	case "VolumeRatio":
		return b.VolumeRatio
	case "TotalMV":
		return b.TotalMV
	}
	return 0
}

func safeFinaFloat(f *model.FinaIndicator, field string) float64 {
	if f == nil {
		return 0
	}
	switch field {
	case "EPS":
		return f.EPS
	case "ROE":
		return f.ROE
	case "GrossProfitMargin":
		return f.GrossProfitMargin
	case "NetProfitMargin":
		return f.NetProfitMargin
	case "DebtToAssets":
		return f.DebtToAssets
	case "NetProfitYoy":
		return f.NetProfitYoy
	}
	return 0
}

func safeBarPctChg(bars []model.Bar) float64 {
	if len(bars) == 0 {
		return 0
	}
	return bars[len(bars)-1].PctChg
}

func filterRelevantNews(news []model.News, name, tsCode string) []model.News {
	code := strings.Split(tsCode, ".")[0]
	var result []model.News
	for _, n := range news {
		if strings.Contains(n.Title, name) || strings.Contains(n.Content, name) ||
			strings.Contains(n.Title, code) || strings.Contains(n.Content, code) {
			result = append(result, n)
		}
	}
	return result
}

func formatNews(news []model.News) string {
	var sb strings.Builder
	for i, n := range news {
		if i >= 8 {
			sb.WriteString(fmt.Sprintf("  ...等共%d条\n", len(news)))
			break
		}
		sb.WriteString(fmt.Sprintf("  [%s] %s\n", n.Datetime, n.Title))
	}
	return sb.String()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

const technicalSysPrompt = `你是专业的A股技术分析师。基于K线、均线、量价数据分析股票短期走势。
重点关注：趋势方向、均线交叉、量价配合、支撑阻力位。
必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`

const fundamentalSysPrompt = `你是专业的A股基本面分析师。基于PE/PB/ROE等财务指标分析股票估值和成长性。
重点关注：估值水平、盈利能力、成长性、财务安全。
必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`

const newsSysPrompt = `你是专业的A股新闻舆情分析师。分析新闻对股票的影响。
重点关注：政策影响、行业事件、公司公告、市场情绪。
必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`

const marketSysPrompt = `你是专业的A股市场分析师。分析大盘环境对个股的影响。
重点关注：指数趋势、市场情绪、系统性风险、板块轮动。
必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`
