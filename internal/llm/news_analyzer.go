package llm

import (
	"encoding/json"
	"fmt"
	"strings"

	"jingzhe-trader/internal/model"
)

// NewsAnalysis 新闻分析结果
// LLM 输出的结构化分析数据，作为量化策略的参考信号
type NewsAnalysis struct {
	Sentiment     float64  `json:"sentiment"`      // 情绪分 -1到1 (正/负)
	ImpactLevel   string   `json:"impact_level"`   // 影响程度: high/medium/low
	KeyPoints     []string `json:"key_points"`     // 关键要点
	Risks         []string `json:"risks"`          // 风险提示
	Opportunities []string `json:"opportunities"`  // 机会提示
	RelatedStocks []string `json:"related_stocks"` // 相关股票代码
	Summary       string   `json:"summary"`        // 一句话总结
}

// NewsAnalyzer LLM 新闻分析器
// 负责调用 LLM 对财经新闻进行深度解读
type NewsAnalyzer struct {
	client *Client
}

// NewNewsAnalyzer 创建新闻分析器
func NewNewsAnalyzer(client *Client) *NewsAnalyzer {
	return &NewsAnalyzer{client: client}
}

// AnalyzeNews 分析单条新闻
// 输入: 新闻对象
// 输出: 结构化的新闻分析结果
// 注意: LLM 分析结果仅供参考，不直接作为交易决策依据
func (na *NewsAnalyzer) AnalyzeNews(news *model.News) (*NewsAnalysis, error) {
	if !na.client.IsEnabled() {
		return nil, fmt.Errorf("LLM 未启用")
	}

	systemPrompt := `你是一名专业的A股证券分析师，擅长解读财经新闻对股票市场的影响。
请分析新闻内容，输出结构化的JSON结果，包含以下字段：
- sentiment: 情绪分，-1到1之间的浮点数，正面为正，负面为负
- impact_level: 影响程度，"high" / "medium" / "low"
- key_points: 关键要点数组，3-5条
- risks: 风险提示数组
- opportunities: 机会提示数组
- related_stocks: 可能受影响的股票代码数组 (如 ["600519.SH", "000858.SZ"])
- summary: 一句话总结，不超过50字

只输出JSON，不要其他文字。`

	userPrompt := fmt.Sprintf(`请分析以下新闻：

标题：%s
内容：%s
发布时间：%s
频道：%s

请输出结构化分析结果。`, news.Title, news.Content, news.Datetime, news.Channels)

	result, err := na.client.Chat(systemPrompt, userPrompt)
	if err != nil {
		return nil, err
	}

	// 解析 JSON (处理可能的markdown代码块包裹)
	result = strings.TrimSpace(result)
	result = strings.TrimPrefix(result, "```json")
	result = strings.TrimPrefix(result, "```")
	result = strings.TrimSuffix(result, "```")
	result = strings.TrimSpace(result)

	var analysis NewsAnalysis
	if err := json.Unmarshal([]byte(result), &analysis); err != nil {
		return nil, fmt.Errorf("解析LLM响应失败: %w, 原始内容: %s", err, result)
	}

	return &analysis, nil
}

// BatchAnalyze 批量分析新闻
// 逐条分析以保证准确性，单条失败不影响整体结果
// 注意: 生产环境可优化为分批并发调用
func (na *NewsAnalyzer) BatchAnalyze(newsList []model.News) ([]NewsAnalysis, error) {
	if !na.client.IsEnabled() {
		return nil, fmt.Errorf("LLM 未启用")
	}
	if len(newsList) == 0 {
		return nil, nil
	}

	var results []NewsAnalysis
	for i := range newsList {
		analysis, err := na.AnalyzeNews(&newsList[i])
		if err != nil {
			// 单条失败不影响整体，跳过继续
			continue
		}
		results = append(results, *analysis)
	}
	return results, nil
}
