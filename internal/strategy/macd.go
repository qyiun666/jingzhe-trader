package strategy

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/indicator"
	"jingzhe-trader/internal/model"
)

// MACDStrategy MACD策略
// DIF上穿DEA(金叉)且柱状图为正时买入, DIF下穿DEA(死叉)时卖出
type MACDStrategy struct {
	Fast        int     // 快线周期 (默认12)
	Slow        int     // 慢线周期 (默认26)
	Signal      int     // 信号线周期 (默认9)
	HistoryLen  int     // 历史数据长度
	PositionPct float64 // 单票仓位占比
}

func (s *MACDStrategy) Name() string { return "macd" }

func (s *MACDStrategy) Init(_ context.Context, params map[string]interface{}) error {
	s.Fast = paramInt(params, "fast", 12)
	s.Slow = paramInt(params, "slow", 26)
	s.Signal = paramInt(params, "signal", 9)
	s.PositionPct = paramFloat(params, "position_pct", 0.1)
	s.HistoryLen = s.Slow + s.Signal + 50
	return nil
}

func (s *MACDStrategy) OnBar(_ context.Context, barCtx *BarContext) ([]model.Signal, error) {
	var signals []model.Signal

	for _, tsCode := range barCtx.Universe {
		closes, err := barCtx.History.GetCloses(tsCode, barCtx.TradeDate, s.HistoryLen)
		if err != nil || len(closes) < s.Slow+s.Signal {
			continue
		}

		macdResult := indicator.MACD(closes, s.Fast, s.Slow, s.Signal)
		n := len(closes)

		if !tail2Valid(macdResult.DIF) || !tail2Valid(macdResult.DEA) {
			continue
		}

		currDIF := macdResult.DIF[n-1]
		currDEA := macdResult.DEA[n-1]
		prevDIF := macdResult.DIF[n-2]
		prevDEA := macdResult.DEA[n-2]
		currHist := macdResult.Histogram[n-1]

		pos, hasPosition := barCtx.Positions[tsCode]

		// 金叉: DIF从下方上穿DEA, 且柱状图为正
		isGoldenCross := crossUp(prevDIF, prevDEA, currDIF, currDEA) && currHist > 0
		// 死叉: DIF从上方下穿DEA
		isDeathCross := crossDown(prevDIF, prevDEA, currDIF, currDEA)

		if isGoldenCross && !hasPosition {
			bar, ok := barCtx.Bars[tsCode]
			if !ok || bar.Close <= 0 {
				continue
			}
			qty := calcBuyQty(barCtx.TotalAsset, bar.Close, s.PositionPct)
			if qty > 0 {
				signals = append(signals, buySignal(tsCode, qty, fmt.Sprintf("MACD金叉: DIF=%.4f上穿DEA=%.4f", currDIF, currDEA), 0.7))
			}
		} else if isDeathCross && hasPosition && pos.TotalQty > 0 {
			signals = append(signals, sellSignal(tsCode, pos.TotalQty, fmt.Sprintf("MACD死叉: DIF=%.4f下穿DEA=%.4f", currDIF, currDEA), 0.7))
		}
	}

	return signals, nil
}
