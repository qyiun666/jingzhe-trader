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
		// 返回缺失占位而非 Confidence=0.1 的空报告: 后者会被当成一张偏空的票计入均值
		return noDataReport(a.Name(), ctx.TsCode, fmt.Sprintf("K线仅%d根, 不足20根无法判断技术形态", len(bars))), nil
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

	flowText := formatMoneyFlows(ctx.MoneyFlows, 5)
	topText := formatTopLists(ctx.TopLists, 3)
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
近5日K线:
%s
最新收盘: %.2f  前收: %.2f  涨跌幅: %.2f%%
MA5: %s  MA20: %s  (MA5%sMA20)
RSI(14): %s  (RSI>70超买, <30超卖)
成交量比(5日均量): %.2f
资金面(近5个交易日):
%s
龙虎榜(近2周):
%s
总资产: %.0f  持仓: %s

请从技术面分析该股票，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		formatRecentBars(bars, 5),
		last.Close, last.PreClose, last.PctChg,
		ma5Str, ma20Str, crossIndicator,
		rsiStr,
		volRatio,
		flowText, topText,
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
	if err != nil {
		return nil, fmt.Errorf("查询 %s 基本面失败: %w", ctx.TsCode, err)
	}
	if len(basics) > 0 {
		basic = &basics[0]
	}
	finas, err := a.finaRepo.GetByCode(ctx.TsCode)
	if err != nil {
		return nil, fmt.Errorf("查询 %s 财务指标失败: %w", ctx.TsCode, err)
	}
	var fina *model.FinaIndicator
	if len(finas) > 0 {
		fina = &finas[0]
	}
	if basic == nil && fina == nil {
		return noDataReport(a.Name(), ctx.TsCode, "无当日基本面与财务指标数据"), nil
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
%s
%s

请从基本面分析该股票投资价值，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}
缺失的一栏请按"信息不足"处理，不得当作 0 值指标解读。`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, basicLine(basic), finaLine(fina))
	return callLLMCommon(a.llm, ctx.TsCode, ctx.TradeDate, "fundamental", fundamentalSysPrompt, userPrompt)
}

// basicLine 估值/成交一行; 无当日 daily_basic 时明写缺失
// 不能输出 PE=0.0 PB=0.00: 模型会把 0 当成真实测量值, 判成"亏损/异常低估"给出方向错误的结论
func basicLine(b *model.DailyBasic) string {
	if b == nil {
		return "基本面: 当日无数据 (缺失, 非 0)"
	}
	return fmt.Sprintf("基本面: PE=%.1f PE_TTM=%.1f PB=%.2f 换手率=%.2f%% 量比=%.2f 市值=%.0f万",
		b.PE, b.PE_TTM, b.PB, b.TurnoverRate, b.VolumeRatio, b.TotalMV)
}

// finaLine 财务一行; 无财报数据时明写缺失
func finaLine(f *model.FinaIndicator) string {
	if f == nil {
		return "财务: 无数据 (缺失, 非 0)"
	}
	return fmt.Sprintf("财务: EPS=%.2f ROE=%.2f%% 毛利率=%.1f%% 净利率=%.1f%% 资产负债率=%.1f%% 营收增速=%.1f%%",
		f.EPS, f.ROE, f.GrossProfitMargin, f.NetProfitMargin, f.DebtToAssets, f.NetProfitYoy)
}

type NewsAnalyst struct {
	llm      *llm.Client
	newsRepo *store.NewsRepo
}

func NewNewsAnalyst(llmClient *llm.Client, newsRepo *store.NewsRepo) *NewsAnalyst {
	return &NewsAnalyst{llm: llmClient, newsRepo: newsRepo}
}
func (a *NewsAnalyst) Name() string { return "news" }

// newsWindowDays 个股相关新闻回溯自然日数 (消息面时效性强于财报, 一周足够)
const newsWindowDays = 7

// newsLimit 喂给模型的相关新闻条数上限
const newsLimit = 20

func (a *NewsAnalyst) Analyze(ctx *DebateContext) (*AnalysisReport, error) {
	keywords := newsKeywords(ctx.Name, ctx.TsCode, ctx.Industry)
	news, err := a.newsRepo.GetMatching(dashedDate(dateMinusDays(ctx.TradeDate, newsWindowDays)), keywords, newsLimit)
	if err != nil {
		return nil, fmt.Errorf("检索 %s 相关新闻失败: %w", ctx.TsCode, err)
	}
	if len(news) == 0 {
		// 无相关新闻按"该维度无输入"处理: 拿全局热点充数会误导辩论, 当成偏空意见同样错
		return noDataReport(a.Name(), ctx.TsCode, fmt.Sprintf("近%d日无该股及其行业(%s)相关新闻",
			newsWindowDays, orUnknown(ctx.Industry))), nil
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  所属行业: %s  日期: %s
相关新闻 (按名称/代码/行业检索, 共%d条, 最新在前; 其中多数可能只是行业或宏观消息):
%s
请分析新闻对%s的影响，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, orUnknown(ctx.Industry), ctx.TradeDate, len(news), formatNews(news), ctx.Name)
	return callLLMCommon(a.llm, ctx.TsCode, ctx.TradeDate, "news", newsSysPrompt, userPrompt)
}

// newsKeywords 新闻检索关键字: 股票简称 + 六位代码 + 所属行业
// 行业是本数据档位下唯一稳定可得的消息面入口 (个股级新闻接口无权限, major_news 只给宏观/行业条目)
func newsKeywords(name, tsCode, industry string) []string {
	return []string{name, strings.Split(tsCode, ".")[0], industry}
}

// orUnknown 空字段的可读占位, 避免提示词里出现空括号
func orUnknown(s string) string {
	if s == "" {
		return "未知"
	}
	return s
}

type MarketAnalyst struct {
	llm        *llm.Client
	repo       *store.BarRepo
	indexCodes []string // 参与判断的大盘指数, 由 dataloader.watchlist 注入
}

func NewMarketAnalyst(llmClient *llm.Client, repo *store.BarRepo, indexCodes []string) *MarketAnalyst {
	return &MarketAnalyst{llm: llmClient, repo: repo, indexCodes: indexCodes}
}
func (a *MarketAnalyst) Name() string { return "market" }

// indexLookbackDays 大盘趋势取近多少个自然日 (约 5 个交易日)
const indexLookbackDays = 12

func (a *MarketAnalyst) Analyze(ctx *DebateContext) (*AnalysisReport, error) {
	var lines []string
	for _, code := range a.indexCodes {
		line, err := a.indexLine(code, ctx.TradeDate)
		if err != nil {
			return nil, fmt.Errorf("查询指数 %s 日线失败: %w", code, err)
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	// 一个有效指数都没有: 让 LLM 判断大盘只会得到编造的结论, 直接标记缺失 (也省一次调用)
	if len(lines) == 0 {
		return noDataReport(a.Name(), ctx.TsCode,
			fmt.Sprintf("无当日大盘指数行情 (期望 %v), 需确认指数日线同步", a.indexCodes)), nil
	}
	_, stockPct := barQuote(ctx.Bars)
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
大盘指数:
%s
该股当日涨跌: %.2f%%

请分析当前市场环境对该股的影响，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, strings.Join(lines, "\n"), stockPct)
	return callLLMCommon(a.llm, ctx.TsCode, ctx.TradeDate, "market", marketSysPrompt, userPrompt)
}

// indexLine 单个指数的近期走势一行; 无当日数据时返回空串 (不把旧行情说成当日)
func (a *MarketAnalyst) indexLine(code, tradeDate string) (string, error) {
	bars, err := a.repo.GetBars(code, dateMinusDays(tradeDate, indexLookbackDays), tradeDate)
	if err != nil {
		return "", err
	}
	if len(bars) == 0 || bars[len(bars)-1].TradeDate != tradeDate {
		return "", nil
	}
	last := bars[len(bars)-1]
	start := 0
	if len(bars) > 5 {
		start = len(bars) - 5
	}
	first := bars[start]
	chg := 0.0
	if first.Close > 0 {
		chg = (last.Close - first.Close) / first.Close * 100
	}
	return fmt.Sprintf("  %s: 收盘%.2f 当日%+.2f%% 近%d个交易日%+.2f%%",
		code, last.Close, last.PctChg, len(bars)-start, chg), nil
}

func callLLMCommon(client *llm.Client, tsCode, tradeDate, agentType, sysPrompt, userPrompt string) (*AnalysisReport, error) {
	report, err := callLLMJSON[AnalysisReport](client, tsCode, tradeDate, agentType, sysPrompt, userPrompt)
	if err != nil {
		return nil, err
	}
	report.Agent = agentType
	report.TsCode = tsCode
	report.Sentiment = clamp(report.Sentiment, -1, 1)
	report.Confidence = clamp(report.Confidence, 0, 1)
	return report, nil
}

// callLLMJSON 通用 LLM 调用样板: 校验启用 → ChatWithCache → 剥离代码块 → 解析 JSON
// 分析师/研究员/风控经理共用, 消除三处重复实现
func callLLMJSON[T any](client *llm.Client, tsCode, tradeDate, role, sysPrompt, userPrompt string) (*T, error) {
	if client == nil || !client.IsEnabled() {
		return nil, fmt.Errorf("LLM 未启用")
	}
	resp, err := client.ChatWithCache(tradeDate, tsCode, role, sysPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM 调用失败: %w", err)
	}
	var out T
	if err := json.Unmarshal([]byte(llm.StripCodeFence(resp)), &out); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w, raw: %.200s", err, resp)
	}
	return &out, nil
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

// barQuote 最后一根K线的收盘价与当日涨跌幅; 无K线时返回 (0,0) 表示行情未知
func barQuote(bars []model.Bar) (close, pctChg float64) {
	if len(bars) == 0 {
		return 0, 0
	}
	last := bars[len(bars)-1]
	return last.Close, last.PctChg
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

const technicalSysPrompt = `你是专业的A股技术分析师，擅长通过K线形态、均线系统和量价关系判断短期走势。

分析框架：
1. 趋势判断：MA5与MA20的多空排列，价格在均线之上为多头，之下为空头
2. 量价配合：放量上涨=资金入场，缩量下跌=抛压减轻，放量下跌=主力出逃
3. K线形态：近5日K线的实体长短、上下影线、连续阳/阴线
4. 支撑阻力：近期高低点形成的关键位
5. 资金面：主力净流入连续为正=资金入场，连续净流出=主力撤退；上龙虎榜的股票短期波动放大，需警惕游资一日游

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
- 区分两类消息：直接提到该股（名称或代码）的个股消息，与只涉及其行业/宏观的行业消息
- 只有个股消息才足以支撑较强方向判断；行业消息最多给 ±0.3，且需在 key_points 里写明是行业层面
- 新闻有时效性，3天内的新闻权重高于1周前的
- 如果没有相关新闻，明确说"无相关新闻"，不要编造

评分标准：
- sentiment: -1(重大利空)到1(重大利好)，0.3以上为偏多，-0.3以下为偏空；仅有行业消息时不超过 ±0.3
- confidence: 0到1，仅有行业消息或消息与该股本弱相关时给0.2-0.4
- key_points: 1-3条，每条须能对应到具体新闻标题或时间
- risks: 1-2条消息面风险（如传闻未证实、利好已兑现、减持公告待落地）

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
