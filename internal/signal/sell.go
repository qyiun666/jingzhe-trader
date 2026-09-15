package signal

import (
	"fmt"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
)

// 五条卖出规则的 rule 取值（验收 #5：各有独立单测）。
const (
	RuleStopLoss     = "stop_loss"     // 止损
	RuleTrailingStop = "trailing_stop" // 移动止盈
	RuleTakeProfit   = "take_profit"   // 止盈
	RuleRankOut      = "rank_out"      // 排名淘汰
	RuleMarketBad    = "market_bad"    // 大盘恶化
)

// HoldingCtx 单只持仓的卖出规则输入。
type HoldingCtx struct {
	Pos       model.Position
	LastClose model.Fen // 最近可得收盘（停牌股取停牌前收盘，分）
	LastDate  string    // 该收盘对应交易日
	InTopN    bool      // 是否在当日候选池 TopN
	MarketBad bool      // 大盘恶化（指数收盘 < MA60）
}

// newSell 构造卖出信号骨架。
func newSell(date string, h HoldingCtx, rule, reason string) *model.Signal {
	name := h.Pos.TsCode // 名称由调用方（service）补齐
	return &model.Signal{
		TradeDate:  date,
		TsCode:     h.Pos.TsCode,
		Name:       name,
		Direction:  model.DirSell,
		Rule:       rule,
		Confidence: 1.0,
		RefPrice:   h.LastClose,
		Reason:     reason,
	}
}

// evalStopLoss 规则 1 止损：最新收盘 ≤ 成本 × (1 − 止损线)。
func evalStopLoss(date string, h HoldingCtx, p risk.RiskParams) *model.Signal {
	if h.Pos.TotalQty <= 0 || h.Pos.CostPrice <= 0 || h.LastClose <= 0 {
		return nil
	}
	trigger := model.Fen(float64(h.Pos.CostPrice) * (1 - p.StopLossPct))
	if h.LastClose > trigger {
		return nil
	}
	pct := (float64(h.Pos.CostPrice-h.LastClose) / float64(h.Pos.CostPrice)) * 100
	return newSell(date, h, RuleStopLoss,
		fmt.Sprintf("止损：现价 %s 跌破止损线 %s（成本 %s，浮亏 %.1f%%，止损线 %.0f%%）",
			h.LastClose, trigger, h.Pos.CostPrice, pct, p.StopLossPct*100))
}

// evalTrailingStop 规则 2 移动止盈：曾创高点（≥成本×(1+2×回撤线)）且回撤超过回撤线。
func evalTrailingStop(date string, h HoldingCtx, p risk.RiskParams) *model.Signal {
	if h.Pos.TotalQty <= 0 || h.Pos.HighPrice <= 0 || h.LastClose <= 0 {
		return nil
	}
	highGate := model.Fen(float64(h.Pos.HighPrice) * (1 - p.TrailingStopPct))
	if h.LastClose > highGate {
		return nil
	}
	// 仅对已盈利的仓位做移动止盈（亏损仓位走止损规则）
	if h.Pos.HighPrice < h.Pos.CostPrice {
		return nil
	}
	drawdown := (float64(h.Pos.HighPrice-h.LastClose) / float64(h.Pos.HighPrice)) * 100
	return newSell(date, h, RuleTrailingStop,
		fmt.Sprintf("移动止盈：现价 %s 自高点 %s 回撤 %.1f%%（超过 %.0f%%）",
			h.LastClose, h.Pos.HighPrice, drawdown, p.TrailingStopPct*100))
}

// evalTakeProfit 规则 3 止盈：最新收盘 ≥ 成本 × (1 + 止盈线)。
func evalTakeProfit(date string, h HoldingCtx, p risk.RiskParams) *model.Signal {
	if h.Pos.TotalQty <= 0 || h.Pos.CostPrice <= 0 || h.LastClose <= 0 {
		return nil
	}
	trigger := model.Fen(float64(h.Pos.CostPrice) * (1 + p.TakeProfitPct))
	if h.LastClose < trigger {
		return nil
	}
	gain := (float64(h.LastClose-h.Pos.CostPrice) / float64(h.Pos.CostPrice)) * 100
	return newSell(date, h, RuleTakeProfit,
		fmt.Sprintf("止盈：现价 %s 达到止盈线 %s（成本 %s，浮盈 %.1f%%，止盈线 %.0f%%）",
			h.LastClose, trigger, h.Pos.CostPrice, gain, p.TakeProfitPct*100))
}

// evalRankOut 规则 4 排名淘汰：持仓未进入当日候选池 TopN。
func evalRankOut(date string, h HoldingCtx) *model.Signal {
	if h.Pos.TotalQty <= 0 || h.InTopN {
		return nil
	}
	return newSell(date, h, RuleRankOut,
		fmt.Sprintf("排名淘汰：未进入当日候选池 TopN（持仓成本 %s）", h.Pos.CostPrice))
}

// evalMarketBad 规则 5 大盘恶化：指数收盘跌破 MA60。
func evalMarketBad(date string, h HoldingCtx, indexClose, indexMA60 model.Fen) *model.Signal {
	if h.Pos.TotalQty <= 0 || indexClose <= 0 || indexMA60 <= 0 {
		return nil
	}
	if indexClose >= indexMA60 {
		return nil
	}
	return newSell(date, h, RuleMarketBad,
		fmt.Sprintf("大盘恶化：指数收盘 %s 元跌破 MA60 %s 元", fmtYuan(indexClose), fmtYuan(indexMA60)))
}

// fmtYuan 分 → 元展示串。
func fmtYuan(f model.Fen) string { return fmt.Sprintf("%.2f", float64(f)/100) }

// EvalSell 按优先级评估五条卖出规则，返回首个触发的信号（未触发返回 nil）：
// 止损 > 移动止盈 > 止盈 > 排名淘汰 > 大盘恶化。
//
// 这是 16:30 收盘后的一次性卖出判定：排名淘汰与大盘恶化都是日频重平衡概念，
// 只有在这一刻才有完整口径（当日 TopN 候选与指数收盘）。
func EvalSell(date string, h HoldingCtx, p risk.RiskParams, indexClose, indexMA60 model.Fen) *model.Signal {
	for _, sig := range []*model.Signal{
		evalStopLoss(date, h, p),
		evalTrailingStop(date, h, p),
		evalTakeProfit(date, h, p),
		evalRankOut(date, h),
		evalMarketBad(date, h, indexClose, indexMA60),
	} {
		if sig != nil {
			return sig
		}
	}
	return nil
}

// EvalPriceRules 盘中每 5 分钟用的卖出判定：只跑三条**价格型**规则
// （止损 > 移动止盈 > 止盈），不碰排名淘汰与大盘恶化。
//
// 不接那两条是刻意的：当日 TopN 候选只在 16:30 内存里（不落库），盘中拿不到就
// 不能伪造 InTopN；大盘 MA60 用的收盘价现算，盘中重算会随价格漂移、把日频熔断
// 变成分钟级抖动。三条价格型规则的输入全部来自持仓成本/高点与实时现价，故可安全盘中跑。
func EvalPriceRules(date string, h HoldingCtx, p risk.RiskParams) *model.Signal {
	for _, sig := range []*model.Signal{
		evalStopLoss(date, h, p),
		evalTrailingStop(date, h, p),
		evalTakeProfit(date, h, p),
	} {
		if sig != nil {
			return sig
		}
	}
	return nil
}
