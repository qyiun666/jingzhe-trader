package strategy

import (
	"context"
	"fmt"
	"math"

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
	s.Fast = 12
	s.Slow = 26
	s.Signal = 9
	s.PositionPct = 0.1
	s.HistoryLen = 100

	if v, ok := params["fast"]; ok {
		if n, ok := v.(float64); ok {
			s.Fast = int(n)
		}
	}
	if v, ok := params["slow"]; ok {
		if n, ok := v.(float64); ok {
			s.Slow = int(n)
		}
	}
	if v, ok := params["signal"]; ok {
		if n, ok := v.(float64); ok {
			s.Signal = int(n)
		}
	}
	if v, ok := params["position_pct"]; ok {
		if n, ok := v.(float64); ok {
			s.PositionPct = n
		}
	}
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

		if math.IsNaN(macdResult.DIF[n-1]) || math.IsNaN(macdResult.DEA[n-1]) ||
			math.IsNaN(macdResult.DIF[n-2]) || math.IsNaN(macdResult.DEA[n-2]) {
			continue
		}

		currDIF := macdResult.DIF[n-1]
		currDEA := macdResult.DEA[n-1]
		prevDIF := macdResult.DIF[n-2]
		prevDEA := macdResult.DEA[n-2]
		currHist := macdResult.Histogram[n-1]

		pos, hasPosition := barCtx.Positions[tsCode]

		// 金叉: DIF从下方上穿DEA, 且柱状图为正
		isGoldenCross := prevDIF <= prevDEA && currDIF > currDEA && currHist > 0
		// 死叉: DIF从上方下穿DEA
		isDeathCross := prevDIF >= prevDEA && currDIF < currDEA

		if isGoldenCross && !hasPosition {
			bar, ok := barCtx.Bars[tsCode]
			if !ok || bar.AdjClose() <= 0 {
				continue
			}
			targetAmount := barCtx.TotalAsset * s.PositionPct
			qty := int(targetAmount/bar.AdjClose()/100) * 100
			if qty > 0 {
				signals = append(signals, model.Signal{
					TsCode:    tsCode,
					Direction: model.DirBuy,
					TargetQty: qty,
					Reason:    fmt.Sprintf("MACD金叉: DIF=%.4f上穿DEA=%.4f", currDIF, currDEA),
					Strength:  0.7,
				})
			}
		} else if isDeathCross && hasPosition && pos.TotalQty > 0 {
			signals = append(signals, model.Signal{
				TsCode:    tsCode,
				Direction: model.DirSell,
				TargetQty: pos.TotalQty,
				Reason:    fmt.Sprintf("MACD死叉: DIF=%.4f下穿DEA=%.4f", currDIF, currDEA),
				Strength:  0.7,
			})
		}
	}

	return signals, nil
}
