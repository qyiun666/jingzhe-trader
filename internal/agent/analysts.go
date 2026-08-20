package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"jingzhe-trader/internal/indicator"
	"jingzhe-trader/internal/llm"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
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
	if len(bars) < 20 {
		// 需至少 20 根 K 线才能计算 MA20, 否则 ma20=0 会导致误导性的"金叉"信号
		return &AnalysisReport{Agent: a.Name(), TsCode: ctx.TsCode, Confidence: 0.1}, nil
	}
	last := bars[len(bars)-1]
	closes := extractCloses(bars)
	ma5 := lastValid(indicator.SMA(closes, 5))
	ma20 := lastValid(indicator.SMA(closes, 20))
	volRatio := 0.0
	if len(bars) > 6 && avgVol(bars[len(bars)-6:len(bars)-1], 5) > 0 {
		volRatio = last.Vol / avgVol(bars[len(bars)-6:len(bars)-1], 5)
	}
	rsi := lastValid(indicator.RSI(closes, 14))
	
	// 处理数据不足的情况
	ma5Str := "N/A"
	ma20Str := "N/A"
	crossIndicator := "未知"
	if !math.IsNaN(ma5) && !math.IsNaN(ma20) {
		ma5Str = fmt.Sprintf("%.2f", ma5)
		ma20Str = fmt.Sprintf("%.2f", ma20)
		crossIndicator = crossStr(ma5, ma20)
	}
	rsiStr := "N/A"
	if !math.IsNaN(rsi) {
		rsiStr = fmt.Sprintf("%.1f", rsi)
	}
	
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
近5日K线:
%s
最新收盘: %.2f  前收: %.2f  涨跌幅: %.2f%%
MA5: %s  MA20: %s  (MA5%sMA20)
RSI(14): %s  (RSI>70超买, <30超卖)
成交量比(5日均量): %.2f
总资产: %.0f  持仓: %s

请从技术面分析该股票，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		formatRecentBars(bars, 5),
		last.Close, last.PreClose, last.PctChg,
		ma5Str, ma20Str, crossIndicator,
		rsiStr,
		volRatio,
		ctx.TotalAsset, posStr(ctx.Position))
	return callLLMCommon(a.llm, ctx.TsCode, ctx.TradeDate, "technical", technicalSysPrompt, userPrompt)
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
	return callLLMCommon(a.llm, ctx.TsCode, ctx.TradeDate, "fundamental", fundamentalSysPrompt, userPrompt)
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
		// 无相关新闻时明确输出, 不拿全局热点充数(无关新闻会误导后续辩论)
		return &AnalysisReport{Agent: a.Name(), TsCode: ctx.TsCode, Confidence: 0.2, KeyPoints: []string{"无相关新闻"}}, nil
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
近期新闻:
%s

请分析新闻对%s的影响，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, formatNews(relevant), ctx.Name)
	return callLLMCommon(a.llm, ctx.TsCode, ctx.TradeDate, "news", newsSysPrompt, userPrompt)
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
	return callLLMCommon(a.llm, ctx.TsCode, ctx.TradeDate, "market", marketSysPrompt, userPrompt)
}

func callLLMCommon(client *llm.Client, tsCode, tradeDate, agentType, sysPrompt, userPrompt string) (*AnalysisReport, error) {
	if client == nil || !client.IsEnabled() {
		return nil, fmt.Errorf("LLM 未启用")
	}
	resp, err := client.ChatWithCache(tradeDate, tsCode, agentType, sysPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM 调用失败: %w", err)
	}
	resp = stripJSON(resp)
	var report AnalysisReport
	report.Agent = agentType
	report.TsCode = tsCode
	if err := json.Unmarshal([]byte(resp), &report); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w, raw: %s", err, resp[:min(200, len(resp))])
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

// lastValid 返回切片中最后一个非 NaN 值, 全 NaN 或空时返回 0
// 用于取 indicator 系列指标 (前 period 个为 NaN) 的最新有效值
// lastValid 返回切片中最后一个非 NaN 值, 无有效值时返回 NaN (调用方判断数据不足)
func lastValid(values []float64) float64 {
	for i := len(values) - 1; i >= 0; i-- {
		if !math.IsNaN(values[i]) {
			return values[i]
		}
	}
	return math.NaN()
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

const technicalSysPrompt = `你是专业的A股技术分析师，擅长通过K线形态、均线系统和量价关系判断短期走势。

分析框架：
1. 趋势判断：MA5与MA20的多空排列，价格在均线之上为多头，之下为空头
2. 量价配合：放量上涨=资金入场，缩量下跌=抛压减轻，放量下跌=主力出逃
3. K线形态：近5日K线的实体长短、上下影线、连续阳/阴线
4. 支撑阻力：近期高低点形成的关键位

A股特性：
- T+1交易，当日买入次日才能卖出
- 涨跌停±10%（ST股±5%），涨停封板买不进，跌停封板卖不出
- 量比>1.5为明显放量，>3为异常放量（需警惕）
- 换手率<1%为僵尸股，>10%可能游资炒作

评分标准：
- sentiment: -1(极度看空)到1(极度看多)，0.3以上为偏多，-0.3以下为偏空
- confidence: 0到1，数据不足时给0.2-0.4
- key_points: 2-3条具体技术分析结论
- risks: 1-2条技术面风险

必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`

const fundamentalSysPrompt = `你是专业的A股基本面分析师，擅长通过财务指标评估股票的估值合理性和成长潜力。

分析框架：
1. 估值水平：PE_TTM<15低估，15-30合理，>50偏贵（科技股可适当放宽）
2. 盈利能力：ROE>15%优秀，毛利率>40%有护城河，净利率>20%盈利强
3. 成长性：营收增速>20%高成长，净利润同比>30%业绩拐点
4. 财务安全：资产负债率<50%稳健，>70%风险较高
5. PB<1破净（可能价值陷阱），PB 1-3合理

注意事项：
- PE为负说明亏损，直接给低sentiment
- 低PE不一定是好事，可能是周期股顶点或价值陷阱
- 高PE不一定是坏事，高成长股值得溢价
- 结合行业属性判断估值合理性

评分标准：
- sentiment: -1到1，PE合理+ROE高+成长性好=0.5以上
- key_points: 具体数值支撑的结论
- risks: 财务风险提示

必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`

const newsSysPrompt = `你是专业的A股新闻舆情分析师，擅长解读财经新闻对个股的短期影响。

分析框架：
1. 政策影响：行业政策、监管动态、财政货币政策对股票的直接影响
2. 公司公告：业绩预告、重大资产重组、股份回购、高管增减持
3. 行业事件：产业链上下游变化、行业拐点、技术突破
4. 市场情绪：机构研报评级变化、北向资金动向、龙虎榜数据

判断标准：
- 利好：政策支持、业绩超预期、大单买入、机构增持
- 利空：监管处罚、业绩不及预期、大股东减持、诉讼仲裁
- 中性：例行公告、常规调研、信息披露

注意：
- 只分析与该股票直接相关的新闻，不发散到行业整体
- 新闻有时效性，3天内的新闻权重高于1周前的
- 如果没有相关新闻，明确说"无相关新闻"，不要编造

必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`

const marketSysPrompt = `你是专业的A股市场宏观分析师，擅长判断大盘环境对个股的系统性影响。

分析框架：
1. 指数趋势：沪深300/上证综指/深证成指的涨跌反映市场整体强弱
2. 市场情绪：大涨说明情绪乐观，大跌说明恐慌，需结合个股属性判断
3. 系统性风险：大盘连续下跌时即使个股优秀也难独善其身
4. 板块轮动：当前市场风格（大盘/小盘、价值/成长）对个股的影响

判断规则：
- 大盘涨+个股涨 = 正常，顺势
- 大盘跌+个股涨 = 个股独立行情，需关注持续性
- 大盘涨+个股跌 = 个股弱于大盘，需警惕
- 大盘跌+个股跌 = 系统性风险，不影响个股基本面判断

注意：
- 大盘涨跌±1%以内为震荡，对个股影响有限
- 大盘±2%以上才需要调整sentiment
- 不要因为大盘短期波动而大幅改变个股长期看法

必须输出合法JSON，格式：{"sentiment": float, "key_points": [], "risks": [], "confidence": float}`
