package analysis

// AdviseStrategy 单元测试: 验证真实策略业绩 + 指数收益序列接入后
// 推荐结果随市况变化且不崩溃 (回归: 之前永远推荐"空仓")

import (
	"testing"

	"jingzhe-trader/internal/model"
)

func TestAdviseStrategy_WithPerformanceData(t *testing.T) {
	perfs := map[string]StrategyPerformance{
		"ma_cross":     {Name: "ma_cross", Sharpe: 0.8, WinRate: 0.45, Recent7Days: 0.01, Recent30Days: 0.03, MaxDrawdown: 0.10},
		"macd":         {Name: "macd", Sharpe: 0.3, WinRate: 0.4, Recent7Days: 0.005, Recent30Days: 0.01, MaxDrawdown: 0.15},
		"multi_factor": {Name: "multi_factor", Sharpe: 0.5, WinRate: 0.5, Recent7Days: 0.0, Recent30Days: 0.02, MaxDrawdown: 0.12},
	}

	// 牛市: 最近30日累计上涨 + 短期趋势向上
	bull := makeReturns(30, 0.002)
	advice := AdviseStrategy("20260817", nil, bull, perfs)
	if advice.RecommendedStrategy == "空仓" {
		t.Errorf("牛市不应推荐空仓, 推荐: %s (市况: %s)", advice.RecommendedStrategy, advice.MarketCondition)
	}
	if advice.Confidence <= 0 || advice.Confidence > 1 {
		t.Errorf("置信度应在(0,1], 实际 %.2f", advice.Confidence)
	}

	// 下跌市: 推荐应为空仓 (防御优先)
	bear := makeReturns(30, -0.002)
	advice2 := AdviseStrategy("20260817", nil, bear, perfs)
	if advice2.MarketCondition != "下跌" {
		t.Errorf("下跌序列应判定为下跌市, 实际 %s", advice2.MarketCondition)
	}
	if advice2.RecommendedStrategy != "空仓" {
		t.Errorf("下跌市应推荐空仓, 实际 %s", advice2.RecommendedStrategy)
	}

	// 无业绩数据: 中性基准, 牛市推荐 macd/ma_cross (不崩溃, 不推荐空仓)
	advice3 := AdviseStrategy("20260817", nil, bull, nil)
	if advice3.RecommendedStrategy == "" || advice3.RecommendedStrategy == "空仓" {
		t.Errorf("牛市无业绩数据时不应推荐空仓/空推荐: %s", advice3.RecommendedStrategy)
	}
}

func TestJudgeMarketCondition_ShortData(t *testing.T) {
	// 数据不足(3天)时按当日涨跌判断, 不崩溃
	short := []float64{0.01, -0.005}
	c := judgeMarketCondition(short, nil)
	if c == "" {
		t.Error("数据不足时也应给出市况判断")
	}
	// 大跌日判定下跌
	short2 := []float64{0.01, -0.03}
	c2 := judgeMarketCondition(short2, map[string]*model.Bar{"000300.SH": {PctChg: -2.5}})
	if c2 != "下跌" {
		t.Errorf("当日大跌应判定下跌, 实际 %s", c2)
	}
}

// makeReturns 构造 N 个日收益率序列 (每日固定涨跌)
func makeReturns(n int, daily float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = daily
	}
	return out
}
