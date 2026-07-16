package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StockAdvice LLM 选股建议
// 注意: 这只是参考建议，最终决策以量化模型为准
// LLM 不直接做交易决策，仅提供分析辅助
type StockAdvice struct {
	StockCode   string   `json:"stock_code"`   // 股票代码
	StockName   string   `json:"stock_name"`   // 股票名称
	BuyReasons  []string `json:"buy_reasons"`  // 看好的理由
	SellReasons []string `json:"sell_reasons"` // 风险/看空的理由
	RiskLevel   string   `json:"risk_level"`   // 风险等级 low/medium/high
	TimeHorizon string   `json:"time_horizon"` // 建议持仓周期 short/medium/long
	Confidence  float64  `json:"confidence"`   // 置信度 0-1
	Summary     string   `json:"summary"`      // 一句话总结
}

// StockAdvisor LLM 选股顾问
// 基于基本面和技术面数据，提供分析参考
type StockAdvisor struct {
	client *Client
}

// NewStockAdvisor 创建选股顾问
func NewStockAdvisor(client *Client) *StockAdvisor {
	return &StockAdvisor{client: client}
}

// AnalyzeStock 分析单只股票的投资价值
// 输入: 股票代码、名称、基本面数据摘要、技术面数据摘要
// 输出: LLM 的分析建议（仅供参考，不构成投资建议）
func (sa *StockAdvisor) AnalyzeStock(tsCode, name string, fundamental string, technical string) (*StockAdvice, error) {
	if !sa.client.IsEnabled() {
		return nil, fmt.Errorf("LLM 未启用")
	}

	systemPrompt := `你是一名专业的A股投资顾问，擅长基本面和技术面综合分析。
请基于提供的数据，给出客观的投资分析建议。
输出JSON格式，包含以下字段：
- stock_code: 股票代码
- stock_name: 股票名称
- buy_reasons: 看好的理由数组 (3-5条)
- sell_reasons: 风险/看空的理由数组 (2-3条)
- risk_level: 风险等级 "low" / "medium" / "high"
- time_horizon: 建议持仓周期 "short" (1-2周) / "medium" (1-3月) / "long" (3月以上)
- confidence: 置信度 0-1 之间
- summary: 一句话总结建议，不超过50字

注意：
1. 保持客观，不要夸大收益
2. 必须提示风险
3. 只输出JSON，不要其他文字`

	userPrompt := fmt.Sprintf(`请分析以下股票：

股票代码：%s
股票名称：%s

基本面数据：
%s

技术面数据：
%s

请给出你的投资分析建议。`, tsCode, name, fundamental, technical)

	result, err := sa.client.Chat(systemPrompt, userPrompt)
	if err != nil {
		return nil, err
	}

	// 清理可能的 markdown 代码块包裹
	result = strings.TrimSpace(result)
	result = strings.TrimPrefix(result, "```json")
	result = strings.TrimPrefix(result, "```")
	result = strings.TrimSuffix(result, "```")
	result = strings.TrimSpace(result)

	var advice StockAdvice
	if err := json.Unmarshal([]byte(result), &advice); err != nil {
		return nil, fmt.Errorf("解析LLM响应失败: %w, 原始内容: %s", err, result)
	}

	return &advice, nil
}
