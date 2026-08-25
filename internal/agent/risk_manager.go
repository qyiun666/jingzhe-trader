package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"jingzhe-trader/internal/llm"
)

type RiskManagerAgent struct {
	llm *llm.Client
}

func NewRiskManagerAgent(client *llm.Client) *RiskManagerAgent {
	return &RiskManagerAgent{llm: client}
}
func (rm *RiskManagerAgent) Name() string { return "risk_manager" }

func (rm *RiskManagerAgent) Judge(ctx *DebateContext, reports []*AnalysisReport, bull, bear *ResearchArgument) (*DebateResult, error) {
	reportsText := formatReports(reports)
	bullText := formatArguments(bull)
	bearText := formatArguments(bear)
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
当前持仓: %s  总资产: %.0f

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
  "position_pct": 0到0.6的仓位建议,
  "stop_price": 止损价(0表示不设),
  "risk_level": "low"或"medium"或"high",
  "bull_args": ["看涨理由1"],
  "bear_args": ["看跌理由1"],
  "risk_note": "风险提示",
  "summary": "50字以内的决策摘要"
}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		posStr(ctx.Position), ctx.TotalAsset,
		reportsText, bullText, bearText)
	raw, err := callLLMJSON[judgeRaw](rm.llm, ctx.TsCode, ctx.TradeDate, "risk_manager", riskMgrSysPrompt, userPrompt)
	if err != nil {
		return nil, err
	}
	result := &DebateResult{
		TradeDate:   ctx.TradeDate,
		TsCode:      ctx.TsCode,
		Name:        ctx.Name,
		Decision:    raw.Decision,
		Confidence:  raw.Confidence,
		PositionPct: raw.PositionPct,
		StopPrice:   raw.StopPrice,
		RiskLevel:   raw.RiskLevel,
		RiskNote:    raw.RiskNote,
		Summary:     raw.Summary,
	}
	if result.Summary == "" {
		result.Summary = fmt.Sprintf("%s: %s (置信度%.0f%%)", ctx.TsCode, raw.Decision, raw.Confidence*100)
	}
	if b, err := json.Marshal(raw.BullArgs); err == nil {
		result.BullArgs = string(b)
	}
	if b, err := json.Marshal(raw.BearArgs); err == nil {
		result.BearArgs = string(b)
	}
	return result, nil
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
	avgSentiment := 0.0
	for _, r := range reports {
		avgSentiment += r.Sentiment
	}
	if len(reports) > 0 {
		avgSentiment /= float64(len(reports))
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
	if blended > 0.15 {
		decision = "buy"
		positionPct = 0.3
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

const riskMgrSysPrompt = `你是专业的风险管理经理，负责最终投资决策。你需要权衡多空双方论点，结合当前持仓和资金状况，做出审慎决策。

决策规则：
- buy: 看涨理由充分（≥2个分析师偏多），技术面+基本面共振，风险可控
- sell: 看跌理由充分，技术面破位或基本面恶化，已持仓时应及时卖出
- hold: 多空不明，维持现状（已持仓继续持有，未持仓继续观望）
- reject: 风险过高（技术面+基本面同时恶化），不应买入

仓位管理（小资金1万级别）：
- position_pct: 0.3-0.4 为标准仓位，0.5-0.6 为重仓（需高置信度）
- 单票不超过总资产60%，保留至少10%现金
- 已有持仓时，新买入信号应降低仓位（避免过度集中）
- stop_price: 建议设在支撑位下方3-5%或成本价下方8%

风险考量：
- A股T+1，买入后当日无法卖出，需承担隔夜风险
- 涨跌停限制可能导致无法及时止损
- 小资金交易成本占比高，频繁交易侵蚀本金
- 若多空分歧大（bull和bear情绪接近），降低置信度

决策标准：
- 4个分析师平均sentiment > 0.3 + 看涨研究员 > 0.4 → 倾向buy
- 4个分析师平均sentiment < -0.2 + 看跌研究员 < -0.3 → 倾向sell/reject
- 已持仓+情绪转负 → 建议 sell
- summary: 必须包含决策核心理由（如"技术面多头+估值合理，建议小仓位买入"）

必须输出合法JSON。`

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
