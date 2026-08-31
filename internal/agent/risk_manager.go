package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"jingzhe-trader/internal/llm"
	"jingzhe-trader/pkg/logger"
)

type RiskManagerAgent struct {
	llm    *llm.Client
	limits PositionLimits
}

// PositionLimits 风控仓位约束, 由组合根从 config.RiskConfig 映射注入
// 必须注入而非写死在提示词里: 此前 prompt 写着「0.5-0.6 重仓 / 单票≤60%」,
// 而实际 risk.max_position_pct=0.40 —— LLM 被鼓励给出必然被风控裁掉的建议
type PositionLimits struct {
	MaxPositionPct      float64 // 单票最大仓位占总资产比例
	MaxTotalPositionPct float64 // 总仓位上限
	StopLossPct         float64 // 止损比例
}

func NewRiskManagerAgent(client *llm.Client, limits PositionLimits) *RiskManagerAgent {
	return &RiskManagerAgent{llm: client, limits: limits}
}
func (rm *RiskManagerAgent) Name() string { return "risk_manager" }

func (rm *RiskManagerAgent) Judge(ctx *DebateContext, reports []*AnalysisReport, bull, bear *ResearchArgument) (*DebateResult, error) {
	reportsText := formatReports(reports)
	bullText := formatArguments(bull)
	bearText := formatArguments(bear)
	price, pctChg := barQuote(ctx.Bars)
	// 历史辩论复盘 (反思闭环): 有该股历史决策记录时注入, 提示 LLM 参考过往决策的实际结果
	reviewSection := ""
	if ctx.ReviewSummary != "" {
		reviewSection = fmt.Sprintf("\n该股历史辩论复盘 (近期有方向决策的实际结果):\n%s请特别参考: 若此前 buy 决策多次亏损, 本次应更保守; 若 sell/reject 后多次踏空上涨, 对看空论点要求更严格证据。\n", ctx.ReviewSummary)
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
现价: %.2f (当日%+.2f%%)  当前持仓: %s  总资产: %.0f
%s
分析师报告:
%s

看涨研究员论点:
%s

看跌研究员论点:
%s

请作为风险管理经理，权衡多空双方论点，做出最终决策。
输出JSON:
{
  "decision": "buy"或"sell"或"hold"或"reject",
  "confidence": 0到1,
  "position_pct": 拟买入金额占总资产的比例, 0~%.2f (上限即单票风控线),
  "stop_price": 决策为 buy 时必须给出低于现价 %.2f 的具体止损价格; 非 buy 决策填 0,
  "risk_level": "low"或"medium"或"high",
  "bull_args": ["看涨理由1"],
  "bear_args": ["看跌理由1"],
  "risk_note": "风险提示",
  "summary": "50字以内的决策摘要"
}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		price, pctChg, posStr(ctx.Position), ctx.TotalAsset,
		reviewSection,
		reportsText, bullText, bearText,
		rm.positionCap(), price)
	raw, err := callLLMJSON[judgeRaw](rm.llm, ctx.TsCode, ctx.TradeDate, "risk_manager", rm.systemPrompt(), userPrompt)
	if err != nil {
		return nil, err
	}
	result := &DebateResult{
		TradeDate:   ctx.TradeDate,
		TsCode:      ctx.TsCode,
		Name:        ctx.Name,
		Decision:    raw.Decision,
		Confidence:  clamp(raw.Confidence, 0, 1),
		PositionPct: clamp(raw.PositionPct, 0, rm.positionCap()),
		StopPrice:   raw.StopPrice,
		RiskLevel:   raw.RiskLevel,
		RiskNote:    raw.RiskNote,
		Summary:     raw.Summary,
	}
	rm.sanitizeStopPrice(result, price)
	if result.Summary == "" {
		result.Summary = fmt.Sprintf("%s: %s (置信度%.0f%%)", ctx.TsCode, raw.Decision, result.Confidence*100)
	}
	if b, err := json.Marshal(raw.BullArgs); err == nil {
		result.BullArgs = string(b)
	}
	if b, err := json.Marshal(raw.BearArgs); err == nil {
		result.BearArgs = string(b)
	}
	return result, nil
}

// sanitizeStopPrice 保证买入决策带有一个可执行的止损价
// 提示词已要求给出低于现价的具体价格; 模型仍可能回 0 (把它当成"不设止损") 或回一个高于现价的价格,
// 这样的价格写进计划原因会误导人工执行, 因此按风控止损线兜底
func (rm *RiskManagerAgent) sanitizeStopPrice(result *DebateResult, price float64) {
	if result.Decision != "buy" {
		result.StopPrice = 0
		return
	}
	if price <= 0 {
		result.StopPrice = 0 // 无现价可比对, 不编造止损价
		return
	}
	if result.StopPrice > 0 && result.StopPrice < price {
		return
	}
	fallback := math.Round(price*(1-rm.stopLossPct())*100) / 100
	logger.L().Warnw("风控裁决止损价不可用, 按止损线兜底", "ts_code", result.TsCode,
		"llm_stop_price", result.StopPrice, "price", price, "fallback", fallback)
	result.StopPrice = fallback
}

// positionCap 单票仓位上限 (提示词与结果裁剪共用同一口径)
func (rm *RiskManagerAgent) positionCap() float64 {
	if rm.limits.MaxPositionPct > 0 {
		return rm.limits.MaxPositionPct
	}
	return 0.2 // 未配置时按保守值, 避免提示词出现 0% 仓位这种无意义区间
}

// stopLossPct 止损线 (提示词与止损价兜底共用)
func (rm *RiskManagerAgent) stopLossPct() float64 {
	if rm.limits.StopLossPct > 0 {
		return rm.limits.StopLossPct
	}
	return 0.08
}

// judgeRaw LLM 风控裁决的原始 JSON 结构 (字段与 DebateResult 对应)
type judgeRaw struct {
	Decision    string   `json:"decision"`
	Confidence  float64  `json:"confidence"`
	PositionPct float64  `json:"position_pct"`
	StopPrice   float64  `json:"stop_price"`
	RiskLevel   string   `json:"risk_level"`
	BullArgs    []string `json:"bull_args"`
	BearArgs    []string `json:"bear_args"`
	RiskNote    string   `json:"risk_note"`
	Summary     string   `json:"summary"`
}

func (rm *RiskManagerAgent) fallbackJudge(ctx *DebateContext, reports []*AnalysisReport, bull, bear *ResearchArgument) *DebateResult {
	// 只对有依据的报告求均值: 缺失报告以 0.0 计入会把结果系统性拉向保守侧
	avgSentiment := 0.0
	valid := 0
	for _, r := range reports {
		if r.IsMissingData() {
			continue
		}
		avgSentiment += r.Sentiment
		valid++
	}
	if valid > 0 {
		avgSentiment /= float64(valid)
	}

	// 结合多空研究员情绪 (bull∈[0,1], bear∈[-1,0], 已在 callResearcherLLM 中 clamp)
	bullSentiment := 0.0
	if bull != nil {
		bullSentiment = bull.Sentiment
	}
	bearSentiment := 0.0
	if bear != nil {
		bearSentiment = bear.Sentiment
	}
	blended := (avgSentiment + bullSentiment + bearSentiment) / 3.0

	// 阈值标定与 prompt 决策标准 (LLM 路径) 对齐:
	// LLM: avg>0.3 + bull>0.4 → buy; avg<-0.2 + bear<-0.3 → sell/reject
	// 降级路径无 bull/bear 明细, 用 blended 近似; 阈值取保守值 (宁缺毋滥)
	decision := "hold"
	positionPct := 0.0
	// 与 systemPrompt 告知 LLM 的口径一致: 常规档 = 风控上限 × 0.6, 绝不超过上限
	stdPct := rm.positionCap() * 0.6
	if valid == 0 {
		// 全部分析师无依据: 不凭多空情绪就给出买入, 宁可观望
		logger.L().Warnw("辩论降级: 无任何有效分析师报告, 维持 hold", "ts_code", ctx.TsCode)
	} else if blended > 0.15 {
		decision = "buy"
		positionPct = stdPct
	} else if blended < -0.15 {
		decision = "reject"
	}

	// 已持仓且情绪极差时建议卖出
	if ctx.Position != nil && blended < -0.35 {
		decision = "sell"
	}

	// 多空分歧越小, 降级置信度越高
	confidence := 0.4
	if bull != nil && bear != nil {
		spread := bullSentiment - bearSentiment
		if spread < 0 {
			spread = -spread
		}
		if spread < 0.5 {
			confidence = 0.45
		}
	}

	var bullArgs, bearArgs []string
	if bull != nil {
		bullArgs = bull.Arguments
	}
	if bear != nil {
		bearArgs = bear.Arguments
	}

	return &DebateResult{
		TradeDate:   ctx.TradeDate,
		TsCode:      ctx.TsCode,
		Name:        ctx.Name,
		Decision:    decision,
		Confidence:  confidence,
		PositionPct: positionPct,
		RiskLevel:   "medium",
		BullArgs:    marshalStrList(bullArgs),
		BearArgs:    marshalStrList(bearArgs),
		RiskNote:    "LLM裁决失败, 使用规则降级(含多空情绪)",
		Summary:     fmt.Sprintf("规则降级: %s (综合情绪%.2f)", decision, blended),
	}
}

func formatArguments(arg *ResearchArgument) string {
	if arg == nil {
		return "无"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("情绪: %.2f  置信度: %.2f\n", arg.Sentiment, arg.Confidence))
	for _, a := range arg.Arguments {
		sb.WriteString(fmt.Sprintf("  - %s\n", a))
	}
	return sb.String()
}

// systemPrompt 按注入的风控参数生成系统提示词
// 仓位与止损数字必须来自配置: 写死过一次 (prompt 建议 0.5-0.6 重仓而 risk.max_position_pct=0.40),
// LLM 就会稳定产出被风控裁掉的建议, 且没人能从结果里看出原因
func (rm *RiskManagerAgent) systemPrompt() string {
	maxPos := rm.positionCap()
	totalCap := rm.totalCap()
	stdPct, heavyPct := maxPos*0.6, maxPos
	cashPct := (1.0 - totalCap) * 100
	stopPct := rm.stopLossPct()

	return fmt.Sprintf(`你是专业的风险管理经理，负责最终投资决策。你需要权衡多空双方论点，结合当前持仓和资金状况，做出审慎决策。

决策规则：
- buy: 看涨理由充分（≥2个分析师偏多），技术面+基本面共振，风险可控
- sell: 看跌理由充分，技术面破位或基本面恶化，已持仓时应及时卖出
- hold: 多空不明，维持现状（已持仓继续持有，未持仓继续观望）
- reject: 风险过高（技术面+基本面同时恶化），不应买入

仓位管理（本系统实际风控约束，超出上限的建议会被风控直接裁掉）：
- position_pct 指"拟买入金额占总资产的比例"，取值 0~%.2f
- 常规仓位约 %.2f，打满上限 %.2f 属重仓（需高置信度才给）
- 总仓位上限 %.0f%%，即至少保留 %.0f%% 现金
- 已有持仓时，新买入信号应降低仓位（避免过度集中）

止损价（stop_price）：
- 决策为 buy 时必须给出一个具体的、低于现价的价格，不要用 0 表示"不设止损"
- 常规取现价下方约 %.0f%%（即本系统止损线），或近期支撑位下方 3-5%%
- 已持仓要卖出时无需给止损价，填 0

风险考量：
- A股T+1，买入后当日无法卖出，需承担隔夜风险
- 涨跌停限制可能导致无法及时止损
- 小资金交易成本占比高，频繁交易侵蚀本金
- 若多空分歧大（bull和bear情绪接近），降低置信度

决策标准：
- 分析师报告标注「数据缺失」的条目不计入均值，也不要当作看空依据
- 有效分析师平均sentiment > 0.3 + 看涨研究员 > 0.4 → 倾向buy
- 有效分析师平均sentiment < -0.2 + 看跌研究员 < -0.3 → 倾向sell/reject
- 已持仓+情绪转负 → 建议 sell
- summary: 必须包含决策核心理由（如"技术面多头+估值合理，建议小仓位买入"）

必须输出合法JSON。`,
		maxPos, stdPct, heavyPct, totalCap*100, cashPct, stopPct*100)
}

// totalCap 总仓位上限 (未配置时按 90% 留出安全垫, 避免提示词出现"总仓位上限 0%")
func (rm *RiskManagerAgent) totalCap() float64 {
	if rm.limits.MaxTotalPositionPct > 0 {
		return rm.limits.MaxTotalPositionPct
	}
	return 0.9
}

// marshalStrList 将字符串列表序列化为 JSON 字符串
func marshalStrList(list []string) string {
	if len(list) == 0 {
		return ""
	}
	b, err := json.Marshal(list)
	if err != nil {
		return ""
	}
	return string(b)
}
