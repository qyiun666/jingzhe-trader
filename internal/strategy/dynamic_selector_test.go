package strategy

import (
	"testing"

	"jingzhe-trader/internal/model"
)

// fixedAdvisor 返回固定推荐的策略建议器
type fixedAdvisor struct{ name string }

func (f fixedAdvisor) Advise(_ string, _ map[string]*model.Bar) *AdvisorResult {
	return &AdvisorResult{RecommendedStrategy: f.name, MarketCondition: "震荡", Confidence: 0.8}
}

// Select 的迟滞计数必须按交易日推进, 不能按调用次数
// Select 同时被只读接口 (日报/策略状态) 调用, 按次数计数会让"连续3日"退化成"被看3次"
func TestSelectHysteresisCountsDistinctDates(t *testing.T) {
	ds := NewDynamicSelector(DefaultRegistry(), fixedAdvisor{name: "boll_breakout"})

	for i := 0; i < 6; i++ {
		name, switched := ds.Select("20260831", nil)
		if switched {
			t.Fatalf("同一日内重复调用第 %d 次发生了切换, 迟滞应按交易日计数", i+1)
		}
		if name != "ma_cross" {
			t.Fatalf("同一日内策略被改动: got %s, want ma_cross", name)
		}
	}

	if _, switched := ds.Select("20260901", nil); switched {
		t.Fatal("第 2 个交易日不应切换 (minHoldDays=3)")
	}
	name, switched := ds.Select("20260902", nil)
	if !switched || name != "boll_breakout" {
		t.Fatalf("连续 3 个交易日后应切换到 boll_breakout, got name=%s switched=%v", name, switched)
	}
}

// 推荐变化时重新起算, 不得沿用旧推荐的累计天数
func TestSelectResetsPendingOnRecommendationChange(t *testing.T) {
	adv := &switchableAdvisor{}
	ds := NewDynamicSelector(DefaultRegistry(), adv)

	adv.name = "macd"
	ds.Select("20260831", nil)
	ds.Select("20260901", nil)
	adv.name = "boll_breakout"
	if _, switched := ds.Select("20260902", nil); switched {
		t.Fatal("推荐变更后应按新推荐重新计数, 不得直接切换")
	}
	ds.Select("20260903", nil)
	if _, switched := ds.Select("20260904", nil); !switched {
		t.Fatal("新推荐连续 3 个交易日后应切换")
	}
}

type switchableAdvisor struct{ name string }

func (a *switchableAdvisor) Advise(_ string, _ map[string]*model.Bar) *AdvisorResult {
	return &AdvisorResult{RecommendedStrategy: a.name, MarketCondition: "震荡", Confidence: 0.8}
}
