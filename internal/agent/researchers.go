package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"jingzhe-trader/internal/llm"
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
		return nil, fmt.Errorf("LLM 未启用")
	}
	resp, err := client.ChatWithCache(tradeDate, tsCode, side, sysPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("LLM 调用失败: %w", err)
	}
	resp = stripJSON(resp)
	var arg ResearchArgument
	if err := json.Unmarshal([]byte(resp), &arg); err != nil {
		return nil, fmt.Errorf("响应解析失败: %w, raw: %s", err, resp[:min(200, len(resp))])
	}
	arg.Side = side // 强制覆盖, 防止 LLM 幻觉翻转多空立场
	// 情绪范围约束: bull 仅允许 [0,1], bear 仅允许 [-1,0] (与 prompt 语义一致, 防御 LLM 越界)
	if side == "bull" {
		arg.Sentiment = math.Max(0, math.Min(1, arg.Sentiment))
	} else if side == "bear" {
		arg.Sentiment = math.Max(-1, math.Min(0, arg.Sentiment))
	}
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

const bullSysPrompt = `你是看涨研究员(Bull)。你的任务是从4位分析师报告中提炼支持买入的核心理由。

工作方法：
1. 优先找出技术面+基本面共振的看多信号（如均线多头+估值合理）
2. 如果技术面偏多但基本面一般，仍可给出偏多建议（技术面领先基本面）
3. 如果新闻面有重大利好（政策/业绩/重组），提高看多置信度
4. arguments 必须具体，引用分析师的数据（如"MA5上穿MA20，量比1.8放量"）

注意事项：
- sentiment 范围 0 到 1（你是看涨方，不应为负）
- 0.3以下表示勉强看多（理由不充分）
- 0.5-0.7 表示中等看多
- 0.8以上表示强烈看多（多重共振）
- 如果分析师报告普遍偏空，你可以给较低的sentiment（如0.2），但不要给负值
- confidence 应反映你对论点的把握程度

必须输出合法JSON：{"side": "bull", "sentiment": float, "arguments": [], "confidence": float}`

const bearSysPrompt = `你是看跌研究员(Bear)。你的任务是从4位分析师报告中提炼风险因素和看空理由。

工作方法：
1. 优先找出技术面破位信号（均线死叉、RSI超买、放量下跌）
2. 基本面风险：高估值(PE>50)、高负债(>70%)、业绩下滑
3. 新闻面利空：监管处罚、大股东减持、业绩不及预期
4. 市场风险：大盘系统性下跌、板块轮动资金流出
5. arguments 必须具体，引用数据（如"PE_TTM 55倍远高于行业平均"）

注意事项：
- sentiment 范围 -1 到 0（你是看跌方，不应为正）
- -0.3以上表示轻度看空（风险可控）
- -0.5 到 -0.7 表示中等看空
- -0.8以下表示强烈看空（多重风险）
- 如果分析师报告普遍偏多，你可以给较低的看空sentiment（如-0.2），但不要给正值
- 如果确实没有明显风险，可以给-0.1（几乎无风险），但必须列出至少一个潜在风险

必须输出合法JSON：{"side": "bear", "sentiment": float, "arguments": [], "confidence": float}`
