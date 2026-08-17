package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"jingzhe-trader/internal/llm"
	"jingzhe-trader/pkg/logger"
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
	if rm.llm == nil || !rm.llm.IsEnabled() {
		return rm.fallbackJudge(ctx, reports, bull, bear), nil
	}
	resp, err := rm.llm.ChatWithCache(ctx.TradeDate, ctx.TsCode, "risk_manager", riskMgrSysPrompt, userPrompt)
	if err != nil {
		logger.L().Warnw("风险管理员LLM调用失败", "ts_code", ctx.TsCode, "err", err)
		return rm.fallbackJudge(ctx, reports, bull, bear), nil
	}
	resp = stripJSON(resp)
	var raw struct {
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
	if err := json.Unmarshal([]byte(resp), &raw); err != nil {
		logger.L().Warnw("风险管理员响应解析失败", "ts_code", ctx.TsCode, "raw", resp[:min(200, len(resp))])
		return rm.fallbackJudge(ctx, reports, bull, bear), nil
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

func (rm *RiskManagerAgent) fallbackJudge(ctx *DebateContext, reports []*AnalysisReport, bull, bear *ResearchArgument) *DebateResult {
	avgSentiment := 0.0
	for _, r := range reports {
		avgSentiment += r.Sentiment
	}
	if len(reports) > 0 {
		avgSentiment /= float64(len(reports))
	}

	// 结合多空研究员情绪
	bullSentiment := 0.0
	if bull != nil {
		bullSentiment = bull.Sentiment
	}
	bearSentiment := 0.0
	if bear != nil {
		bearSentiment = bear.Sentiment
	}
	blended := (avgSentiment + bullSentiment + bearSentiment) / 3.0

	decision := "hold"
	positionPct := 0.0
	if blended > 0.1 {
		decision = "buy"
		positionPct = 0.3
	} else if blended < -0.1 {
		decision = "reject"
	}

	// 已持仓且情绪极差时建议卖出
	if ctx.Position != nil && blended < -0.3 {
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

const riskMgrSysPrompt = `你是专业的风险管理经理。你需要权衡看涨和看跌研究员的论点，做出最终投资决策。
决策规则：
- buy: 看涨理由充分，风险可控
- sell: 看跌理由充分，应及时卖出
- hold: 多空不明，维持现状
- reject: 风险过高，不应买入

仓位建议(position_pct)不超过0.6(60%)。
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
