package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"jingzhe-trader/internal/llm"
	"jingzhe-trader/pkg/logger"
)

type BullResearcher struct{ llm *llm.Client }

func NewBullResearcher(client *llm.Client) *BullResearcher {
	return &BullResearcher{llm: client}
}
func (r *BullResearcher) Name() string { return "bull" }

func (r *BullResearcher) Research(ctx *DebateContext, reports []*AnalysisReport) (*ResearchArgument, error) {
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s  当前持仓: %s

4位分析师报告:
%s

请作为看涨研究员，从上述报告中找出支持买入的理由，输出JSON:
{"side": "bull", "sentiment": 0到1, "arguments": ["理由1","理由2","理由3"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, posStr(ctx.Position), formatReports(reports))
	return callResearcherLLM(r.llm, ctx.TsCode, ctx.TradeDate, "bull", bullSysPrompt, userPrompt)
}

type BearResearcher struct{ llm *llm.Client }

func NewBearResearcher(client *llm.Client) *BearResearcher {
	return &BearResearcher{llm: client}
}
func (r *BearResearcher) Name() string { return "bear" }

func (r *BearResearcher) Research(ctx *DebateContext, reports []*AnalysisReport) (*ResearchArgument, error) {
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s  当前持仓: %s

4位分析师报告:
%s

请作为看跌研究员，从上述报告中找出风险和看空理由，输出JSON:
{"side": "bear", "sentiment": -1到0, "arguments": ["理由1","理由2","理由3"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, posStr(ctx.Position), formatReports(reports))
	return callResearcherLLM(r.llm, ctx.TsCode, ctx.TradeDate, "bear", bearSysPrompt, userPrompt)
}

func callResearcherLLM(client *llm.Client, tsCode, tradeDate, side, sysPrompt, userPrompt string) (*ResearchArgument, error) {
	if client == nil || !client.IsEnabled() {
		return &ResearchArgument{Side: side, Sentiment: 0, Arguments: []string{"LLM未启用"}, Confidence: 0.3}, nil
	}
	resp, err := client.ChatWithCache(tradeDate, tsCode, side, sysPrompt, userPrompt)
	if err != nil {
		logger.L().Warnw("研究员LLM调用失败", "side", side, "ts_code", tsCode, "err", err)
		return &ResearchArgument{Side: side, Sentiment: 0, Arguments: []string{"LLM调用失败"}, Confidence: 0.2}, nil
	}
	resp = stripJSON(resp)
	var arg ResearchArgument
	if err := json.Unmarshal([]byte(resp), &arg); err != nil {
		logger.L().Warnw("研究员响应解析失败", "side", side, "ts_code", tsCode, "raw", resp[:min(200, len(resp))])
		return &ResearchArgument{Side: side, Sentiment: 0, Arguments: []string{"响应解析失败"}, Confidence: 0.2}, nil
	}
	arg.Side = side // 强制覆盖, 防止 LLM 幻觉翻转多空立场
	return &arg, nil
}

func formatReports(reports []*AnalysisReport) string {
	var sb strings.Builder
	for _, r := range reports {
		sb.WriteString(fmt.Sprintf("【%s分析师】 情绪: %.2f 置信度: %.2f\n", r.Agent, r.Sentiment, r.Confidence))
		for _, p := range r.KeyPoints {
			sb.WriteString(fmt.Sprintf("  + %s\n", p))
		}
		for _, risk := range r.Risks {
			sb.WriteString(fmt.Sprintf("  ! %s\n", risk))
		}
	}
	return sb.String()
}

const bullSysPrompt = `你是看涨研究员(Bull)。你的任务是从分析师报告中找出支持买入的理由。
保持客观，不要凭空捏造利好，只基于分析师提供的数据论证。
必须输出合法JSON：{"side": "bull", "sentiment": float, "arguments": [], "confidence": float}`

const bearSysPrompt = `你是看跌研究员(Bear)。你的任务是从分析师报告中找出风险和看空理由。
保持客观，不要凭空捏造利空，只基于分析师提供的数据论证。
必须输出合法JSON：{"side": "bear", "sentiment": float, "arguments": [], "confidence": float}`
