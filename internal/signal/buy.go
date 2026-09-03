package signal

import (
	"context"
	"fmt"
	"math"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
)

// BuyRuleName 买入规则名（signal.rule 取值）。
const BuyRuleName = "buy_trend"

// BuyConfirmer 买入候选终审接口（用户决策：LLM 只做买入候选终审）。
// Batch 3 只提供直接放行的默认实现；Batch 4 接入 DeepSeek 实现（不触网约束由实现方保证）。
type BuyConfirmer interface {
	// Confirm 对单个买入候选做终审。返回 false 表示否决该候选（不产生信号）。
	Confirm(ctx context.Context, candidate BuyCandidate) (bool, error)
}

// BuyCandidate 终审输入：选股结果 + 股票名称。
type BuyCandidate struct {
	Result model.ScreenResult
	Name   string
}

// PassThroughConfirmer 默认终审：直接放行（不触网，Batch 4 替换为 LLM 实现）。
type PassThroughConfirmer struct{}

// Confirm 直接放行。
func (PassThroughConfirmer) Confirm(context.Context, BuyCandidate) (bool, error) {
	return true, nil
}

// BarSeries 单只股票的指标输入（升序）：前复权收盘与成交量（手）。
type BarSeries struct {
	Closes []float64
	Vols   []float64
}

// 买入触发阈值（趋势确认，ARCHITECTURE §2.8 buy.go）。
const (
	volRatioMin = 1.2 // 量能 ≥ 5 日均量 ×1.2
	minBars     = 20  // MA20 所需最少根数
)

// EvalBuy 单候选买入规则评估：
// 趋势确认（MA5 > MA20 且 量比 ≥ 1.2）且置信度 ≥ 档位下限 → 生成买入信号。
// 未触发或数据不足返回 nil。
func EvalBuy(c BuyCandidate, bs BarSeries, p risk.RiskParams) *model.Signal {
	if len(bs.Closes) < minBars || len(bs.Vols) < minBars {
		return nil // 数据不足：不产生信号（由调用方记降级，不静默丢弃候选本身）
	}
	ma5, ma20 := MA5MA20(bs.Closes)
	if math.IsNaN(ma20) || ma5 <= ma20 {
		return nil
	}
	vr := VolumeRatio(bs.Vols)
	if vr < volRatioMin {
		return nil
	}
	confidence := c.Result.Score / 100
	if confidence < p.MinConfidence {
		return nil // 置信度不足：规则未触发（批次风控侧还有同名校验，双保险）
	}
	ref := c.Result.Close
	if ref <= 0 {
		return nil
	}
	sig := &model.Signal{
		TradeDate:  c.Result.TradeDate,
		TsCode:     c.Result.TsCode,
		Name:       c.Name,
		Direction:  model.DirBuy,
		Rule:       BuyRuleName,
		Confidence: confidence,
		RefPrice:   ref,
		Reason: fmt.Sprintf("综合分 %.1f；MA5 %.2f > MA20 %.2f，量比 %.2f（≥%.1f）",
			c.Result.Score, ma5, ma20, vr, volRatioMin),
		Status: "new",
	}
	sig.Payload = fmt.Sprintf(`{"rank":%d,"score":%.2f,"ma5":%.4f,"ma20":%.4f,"vol_ratio":%.4f}`,
		c.Result.Rank, c.Result.Score, ma5, ma20, vr)
	return sig
}
