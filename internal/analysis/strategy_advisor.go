package analysis

import (
	"fmt"
	"sort"
	"strings"

	"jingzhe-trader/internal/model"
)

// StrategyPerformance 策略业绩表现
type StrategyPerformance struct {
	Name         string
	TotalReturn  float64
	Sharpe       float64
	MaxDrawdown  float64
	WinRate      float64
	Recent7Days  float64 // 最近7日收益
	Recent30Days float64 // 最近30日收益
}

// StrategyAdvice 策略建议
type StrategyAdvice struct {
	RecommendedStrategy   string   // 推荐策略名
	Confidence            float64  // 置信度 0-1
	Reason                string   // 推荐理由
	MarketCondition       string   // 市场环境判断
	AlternativeStrategies []string // 备选策略
}

// scoredStrategy 带评分的策略
type scoredStrategy struct {
	name  string
	score float64
	perf  StrategyPerformance
}

// AdviseStrategy 根据市场环境推荐策略
// tradeDate: 当前交易日期
// marketBars: 指数行情 (如 000300.SH), 用于辅助判断当日市场状况
// recentReturns: 最近N日大盘收益率序列
// strategyPerformances: 各策略的历史业绩表现
func AdviseStrategy(
	tradeDate string,
	marketBars map[string]*model.Bar,
	recentReturns []float64,
	strategyPerformances map[string]StrategyPerformance,
) *StrategyAdvice {
	advice := &StrategyAdvice{}

	// 1. 判断市场环境
	marketCondition := judgeMarketCondition(recentReturns, marketBars)
	advice.MarketCondition = marketCondition

	// 2. 根据市场环境确定候选策略及评分权重
	candidates := getCandidateStrategies(marketCondition)

	// 3. 对所有候选策略评分
	var scored []scoredStrategy
	for _, name := range candidates {
		if name == "空仓" {
			scored = append(scored, scoredStrategy{
				name:  "空仓",
				score: calculateEmptyScore(marketCondition),
			})
			continue
		}
		if perf, ok := strategyPerformances[name]; ok {
			score := calculateStrategyScore(perf, marketCondition)
			scored = append(scored, scoredStrategy{name: name, score: score, perf: perf})
		}
	}

	// 按评分降序排列
	sort.Slice(scored, func(i, j int) bool {
		return scored[i].score > scored[j].score
	})

	if len(scored) == 0 {
		advice.RecommendedStrategy = "空仓"
		advice.Confidence = 0.5
		advice.Reason = "缺乏策略业绩数据,建议观望"
		return advice
	}

	// 4. 选取推荐策略
	best := scored[0]
	advice.RecommendedStrategy = best.name
	advice.Confidence = normalizeConfidence(best.score, marketCondition)
	advice.Reason = buildAdviceReason(best, marketCondition, recentReturns)

	// 5. 设置备选策略 (取评分第2-3名的非空仓策略)
	for i := 1; i < len(scored) && len(advice.AlternativeStrategies) < 2; i++ {
		if scored[i].name != advice.RecommendedStrategy {
			advice.AlternativeStrategies = append(advice.AlternativeStrategies, scored[i].name)
		}
	}

	return advice
}

// judgeMarketCondition 判断市场环境
// 根据大盘最近N日收益率序列计算MA5/MA20及近期趋势
func judgeMarketCondition(recentReturns []float64, marketBars map[string]*model.Bar) string {
	// 尝试从指数bar获取当日涨跌辅助判断
	var dailyChg float64
	if bar := marketBars["000300.SH"]; bar != nil {
		dailyChg = bar.PctChg / 100.0
	} else if bar := marketBars["000001.SH"]; bar != nil {
		dailyChg = bar.PctChg / 100.0
	}

	if len(recentReturns) < 5 {
		// 数据不足,结合当日涨跌判断
		if dailyChg < -0.02 {
			return "下跌"
		} else if dailyChg > 0.02 {
			return "牛市"
		}
		return "震荡"
	}

	// 从收益率序列重建价格序列 (以100为基准)
	prices := make([]float64, len(recentReturns)+1)
	prices[0] = 100.0
	for i, r := range recentReturns {
		prices[i+1] = prices[i] * (1 + r)
	}

	// 计算最近5日累计收益
	var recent5DayReturn float64
	start := len(recentReturns) - 5
	if start < 0 {
		start = 0
	}
	for i := start; i < len(recentReturns); i++ {
		recent5DayReturn += recentReturns[i]
	}

	// 计算MA5和MA20
	ma5 := calcMA(prices, 5)
	ma20 := calcMA(prices, 20)

	// 判断逻辑
	if len(prices) >= 20 {
		if ma5 > ma20 && recent5DayReturn > 0 {
			return "牛市"
		}
		if ma20 > ma5 && recent5DayReturn < 0 {
			return "下跌"
		}
	}

	// 短期趋势判断
	if recent5DayReturn < -0.03 || dailyChg < -0.025 {
		return "下跌"
	}
	if recent5DayReturn > 0.02 && dailyChg > 0 {
		return "牛市"
	}

	return "震荡"
}

// calcMA 计算简单移动平均
func calcMA(prices []float64, period int) float64 {
	if len(prices) < period {
		return 0
	}
	var sum float64
	for i := len(prices) - period; i < len(prices); i++ {
		sum += prices[i]
	}
	return sum / float64(period)
}

// getCandidateStrategies 根据市场环境获取候选策略列表
func getCandidateStrategies(condition string) []string {
	switch condition {
	case "牛市":
		// 动量策略优先
		return []string{"macd", "ma_cross", "multi_factor"}
	case "震荡":
		// 均值回归和防御策略
		return []string{"boll_breakout", "multi_factor", "macd"}
	case "下跌":
		// 防御或空仓
		return []string{"multi_factor", "空仓", "boll_breakout"}
	default:
		return []string{"multi_factor", "macd", "boll_breakout"}
	}
}

// calculateStrategyScore 计算策略综合得分
// 不同市场环境下评分权重不同
func calculateStrategyScore(perf StrategyPerformance, marketCondition string) float64 {
	switch marketCondition {
	case "牛市":
		// 牛市: 近期收益权重最高,其次夏普,回撤惩罚较轻
		return perf.Recent7Days*30 +
			perf.Recent30Days*20 +
			perf.Sharpe*5 +
			perf.WinRate*3 -
			perf.MaxDrawdown*5
	case "震荡":
		// 震荡: 夏普和胜率优先,收益权重降低
		return perf.Sharpe*15 +
			perf.WinRate*10 +
			perf.Recent7Days*10 +
			perf.Recent30Days*5 -
			perf.MaxDrawdown*10
	case "下跌":
		// 下跌: 回撤控制最重要,其次夏普和胜率
		return -perf.MaxDrawdown*30 +
			perf.Sharpe*10 +
			perf.WinRate*5 +
			perf.Recent7Days*3 +
			perf.Recent30Days*2
	default:
		return perf.Sharpe*10 + perf.WinRate*5 + perf.Recent7Days*5 - perf.MaxDrawdown*5
	}
}

// calculateEmptyScore 空仓策略的基准得分
// 不同市场环境下的空仓吸引力不同
func calculateEmptyScore(marketCondition string) float64 {
	switch marketCondition {
	case "牛市":
		return -8 // 牛市不应空仓
	case "震荡":
		return 2 // 震荡市空仓有一定吸引力
	case "下跌":
		return 6 // 下跌市空仓是强候选
	default:
		return 0
	}
}

// normalizeConfidence 将原始得分归一化为 0-1 的置信度
func normalizeConfidence(score float64, marketCondition string) float64 {
	// 根据市场环境设定不同的归一化基准
	var base float64
	switch marketCondition {
	case "牛市":
		base = 3.0
	case "震荡":
		base = 2.0
	case "下跌":
		base = 2.0
	default:
		base = 2.0
	}

	confidence := score / base
	if confidence > 1.0 {
		confidence = 1.0
	}
	if confidence < 0 {
		confidence = 0
	}
	return confidence
}

// buildAdviceReason 生成推荐理由文本
func buildAdviceReason(best scoredStrategy, marketCondition string, recentReturns []float64) string {
	var parts []string

	// 市场环境描述
	switch marketCondition {
	case "牛市":
		parts = append(parts, "当前市场处于上升趋势")
	case "震荡":
		parts = append(parts, "当前市场处于震荡格局")
	case "下跌":
		parts = append(parts, "当前市场处于下跌通道")
	}

	// 推荐策略及原因
	if best.name == "空仓" {
		parts = append(parts, "建议空仓观望以规避下行风险")
	} else {
		parts = append(parts, fmt.Sprintf("推荐策略:%s", best.name))

		// 加入近期收益数据
		if best.perf.Recent7Days != 0 {
			parts = append(parts, fmt.Sprintf("该策略近7日收益%+.2f%%", best.perf.Recent7Days*100))
		}
		if best.perf.Recent30Days != 0 {
			parts = append(parts, fmt.Sprintf("近30日收益%+.2f%%", best.perf.Recent30Days*100))
		}
		if best.perf.Sharpe > 0 {
			parts = append(parts, fmt.Sprintf("夏普比率:%.2f", best.perf.Sharpe))
		}
		if best.perf.MaxDrawdown > 0 {
			parts = append(parts, fmt.Sprintf("最大回撤:%.2f%%", best.perf.MaxDrawdown*100))
		}
	}

	// 加入大盘近期走势
	if len(recentReturns) >= 5 {
		var recent5 float64
		for i := len(recentReturns) - 5; i < len(recentReturns); i++ {
			recent5 += recentReturns[i]
		}
		parts = append(parts, fmt.Sprintf("大盘近5日累计收益%+.2f%%", recent5*100))
	}

	return strings.Join(parts, ", ")
}
