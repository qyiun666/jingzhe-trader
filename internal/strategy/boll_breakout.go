package strategy

import (
	"context"
	"fmt"
	"math"

	"jingzhe-trader/internal/indicator"
	"jingzhe-trader/internal/model"
)

// BollBreakoutStrategy 布林带突破策略
// 收盘价突破下轨后回升买入, 突破上轨或回到中轨卖出
type BollBreakoutStrategy struct {
	Period      int     // 布林带周期 (默认20)
	Multiplier  float64 // 标准差倍数 (默认2.0)
	HistoryLen  int     // 历史数据长度
	PositionPct float64 // 单票仓位占比
}

func (s *BollBreakoutStrategy) Name() string { return "boll_breakout" }

func (s *BollBreakoutStrategy) Init(_ context.Context, params map[string]interface{}) error {
	s.Period = 20
	s.Multiplier = 2.0
	s.PositionPct = 0.1
	s.HistoryLen = 80

	if v, ok := params["period"]; ok {
		if n, ok := v.(float64); ok {
			s.Period = int(n)
		}
	}
	if v, ok := params["multiplier"]; ok {
		if n, ok := v.(float64); ok {
			s.Multiplier = n
		}
	}
	if v, ok := params["position_pct"]; ok {
		if n, ok := v.(float64); ok {
			s.PositionPct = n
		}
	}
	s.HistoryLen = s.Period * 4
	return nil
}

func (s *BollBreakoutStrategy) OnBar(_ context.Context, barCtx *BarContext) ([]model.Signal, error) {
	var signals []model.Signal

	for _, tsCode := range barCtx.Universe {
		closes, err := barCtx.History.GetCloses(tsCode, barCtx.TradeDate, s.HistoryLen)
		if err != nil || len(closes) < s.Period+1 {
			continue
		}

		boll := indicator.Boll(closes, s.Period, s.Multiplier)
		n := len(closes)

		if math.IsNaN(boll.Upper[n-1]) || math.IsNaN(boll.Lower[n-1]) || math.IsNaN(boll.Middle[n-1]) {
			continue
		}
		if math.IsNaN(boll.Upper[n-2]) || math.IsNaN(boll.Lower[n-2]) {
			continue
		}

		currClose := closes[n-1]
		prevClose := closes[n-2]
		currUpper := boll.Upper[n-1]
		currLower := boll.Lower[n-1]
		currMiddle := boll.Middle[n-1]
		prevLower := boll.Lower[n-2]

		pos, hasPosition := barCtx.Positions[tsCode]

		// 买入信号: 昨日收盘价在下轨附近, 今日回升突破下轨
		// (昨日触及下轨下方, 今日回升到下轨上方)
		isBuySignal := prevClose <= prevLower && currClose > currLower

		// 卖出信号: 收盘价突破上轨 或 回到中轨下方 (已持仓时)
		isSellSignal1 := currClose >= currUpper
		isSellSignal2 := hasPosition && pos.TotalQty > 0 && currClose < currMiddle

		if isBuySignal && !hasPosition {
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
					Reason:    fmt.Sprintf("布林带下轨回升: close=%.2f > lower=%.2f", currClose, currLower),
					Strength:  0.6,
				})
			}
		} else if (isSellSignal1 || isSellSignal2) && hasPosition && pos.TotalQty > 0 {
			reason := fmt.Sprintf("布林带卖出: close=%.2f", currClose)
			if isSellSignal1 {
				reason = fmt.Sprintf("布林带上轨突破: close=%.2f >= upper=%.2f", currClose, currUpper)
			} else {
				reason = fmt.Sprintf("布林带回到中轨下方: close=%.2f < middle=%.2f", currClose, currMiddle)
			}
			signals = append(signals, model.Signal{
				TsCode:    tsCode,
				Direction: model.DirSell,
				TargetQty: pos.TotalQty,
				Reason:    reason,
				Strength:  0.6,
			})
		}
	}

	return signals, nil
}
