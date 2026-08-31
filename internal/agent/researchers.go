package agent

import (
	"fmt"
	"math"
	"strings"

	"jingzhe-trader/internal/llm"
)

// researcher 多空研究员: 以 side 区分看涨/看跌立场, 共用同一套调用逻辑
type researcher struct {
	llm  *llm.Client
	side string // "bull" / "bear"
}

// researcherPrompt 研究员的 prompt 模板 (按 side 查表, 内容为原 Bull/Bear 两份 SysPrompt 合并)
type researcherPrompt struct {
	sys       string // 系统提示词
	task      string // user prompt 中的任务描述
	jsonField string // 输出 JSON 的字段约束 (sentiment 范围)
}

var researcherPrompts = map[string]researcherPrompt{
	"bull": {
		task: "请作为看涨研究员，从上述报告中找出支持买入的理由",
		sys: `你是看涨研究员(Bull)。你的任务是从4位分析师报告中提炼支持买入的核心理由。

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

必须输出合法JSON：{"side": "bull", "sentiment": float, "arguments": [], "confidence": float}`,
		jsonField: "0到1",
	},
	"bear": {
		task: "请作为看跌研究员，从上述报告中找出风险和看空理由",
		sys: `你是看跌研究员(Bear)。你的任务是从4位分析师报告中提炼风险因素和看空理由。

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

必须输出合法JSON：{"side": "bear", "sentiment": float, "arguments": [], "confidence": float}`,
		jsonField: "-1到0",
	},
}

// NewResearcher 创建研究员 (side: "bull" 看涨 / "bear" 看跌)
func NewResearcher(client *llm.Client, side string) *researcher {
	return &researcher{llm: client, side: side}
}

func (r *researcher) Name() string { return r.side }

func (r *researcher) Research(ctx *DebateContext, reports []*AnalysisReport) (*ResearchArgument, error) {
	p := researcherPrompts[r.side]
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s  当前持仓: %s

4位分析师报告:
%s

%s，输出JSON:
{"side": "%s", "sentiment": %s, "arguments": ["理由1","理由2","理由3"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate, posStr(ctx.Position), formatReports(reports),
		p.task, r.side, p.jsonField)
	arg, err := callLLMJSON[ResearchArgument](r.llm, ctx.TsCode, ctx.TradeDate, r.side, p.sys, userPrompt)
	if err != nil {
		return nil, err
	}
	arg.Side = r.side // 强制覆盖, 防止 LLM 幻觉翻转多空立场
	// 情绪范围约束: bull 仅允许 [0,1], bear 仅允许 [-1,0] (与 prompt 语义一致, 防御 LLM 越界)
	if r.side == "bull" {
		arg.Sentiment = math.Max(0, math.Min(1, arg.Sentiment))
	} else if r.side == "bear" {
		arg.Sentiment = math.Max(-1, math.Min(0, arg.Sentiment))
	}
	arg.Confidence = clamp(arg.Confidence, 0, 1)
	return arg, nil
}

func formatReports(reports []*AnalysisReport) string {
	var sb strings.Builder
	for _, r := range reports {
		if r.IsMissingData() {
			// 不给情绪/置信度数值: 否则下游会把"没数据"读成一条中性偏空的意见
			sb.WriteString(fmt.Sprintf("【%s分析师】 数据缺失, 不计入均值\n", r.Agent))
			continue
		}
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
