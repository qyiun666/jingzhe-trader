package api

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/pkg/logger"
)

// RunRebalance 调仓建议
func (s *Service) RunRebalance(date string, strategyName string) (*RebalanceJSON, error) {
	allBars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return nil, fmt.Errorf("获取行情失败: %w", err)
	}

	todayBars := make(map[string]*model.Bar, len(allBars))
	for i := range allBars {
		b := &allBars[i]
		todayBars[b.TsCode] = b
	}

	positions := s.getPositions()
	asset := s.getAsset()
	s.brk.UpdateMarketValue(todayBars)
	positions, _ = s.brk.QueryPositions()
	asset, _ = s.brk.QueryAsset()

	signals, sigErr := s.runStrategy(date, strategyName, todayBars, positions, asset)
	if sigErr != nil {
		logger.L().Errorf("[%s] 调仓建议策略信号生成失败: %v", date, sigErr)
	}
	return s.buildRebalanceJSON(date, signals, positions, asset, todayBars), nil
}

// buildRebalanceJSON 构建调仓计划 JSON
func (s *Service) buildRebalanceJSON(
	date string,
	signals []model.Signal,
	positions map[string]*model.Position,
	asset *broker.AssetInfo,
	todayBars map[string]*model.Bar,
) *RebalanceJSON {
	totalAsset := asset.TotalAsset
	if totalAsset <= 0 {
		totalAsset = asset.Cash + asset.MarketValue
	}

	// 构建信号 map
	signalMap := make(map[string]model.Signal)
	for _, sig := range signals {
		signalMap[sig.TsCode] = sig
	}

	// 止损/止盈管理器 (阈值由配置驱动, 与风控管道同一套参数)
	sl := risk.NewStopLossManager(s.cfg.Risk.StopLossPct, s.cfg.Risk.TakeProfitPct)
	if s.cfg.Risk.TrailingStopPct > 0 {
		sl.SetTrailingStop(s.cfg.Risk.TrailingStopPct)
	}

	// 遍历持仓, 分类卖出/持有
	var sellList []TradeSuggestionJSON
	var holdList []HoldSuggestionJSON

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		price := pos.MarketPrice
		if bar, ok := todayBars[tsCode]; ok && bar.Close > 0 {
			price = bar.Close
		}
		if price <= 0 {
			continue
		}

		sig, hasSignal := signalMap[tsCode]

		// 止损/止盈检查 (统一走 StopLossManager, 消除与风控管道的规则漂移)
		triggered, reason := sl.CheckSingle(pos, price)
		if triggered {
			sellQty := pos.AvailableQty
			if sellQty > 0 {
				sellList = append(sellList, TradeSuggestionJSON{
					TsCode:   tsCode,
					Name:     s.stockName(tsCode),
					Action:   "sell",
					DeltaQty: -sellQty,
					Price:    price,
					Amount:   price * float64(sellQty),
					Priority: 1,
					Reason:   reason,
					Urgency:  "立即",
				})
			}
			continue
		}

		// 策略信号卖出
		if hasSignal && sig.Direction == model.DirSell {
			sellQty := pos.AvailableQty
			if sellQty > 0 {
				sellList = append(sellList, TradeSuggestionJSON{
					TsCode:   tsCode,
					Name:     s.stockName(tsCode),
					Action:   "sell",
					DeltaQty: -sellQty,
					Price:    price,
					Amount:   price * float64(sellQty),
					Priority: 3,
					Reason:   "策略信号: " + sig.Reason,
					Urgency:  "今日",
				})
			}
			continue
		}

		// 持有
		suggestion := "继续持有"
		if pos.FloatingPnLPct < -0.03 {
			suggestion = "关注止损位"
		} else if pos.FloatingPnLPct > 0.15 {
			suggestion = "接近止盈"
		}
		holdList = append(holdList, HoldSuggestionJSON{
			TsCode:      tsCode,
			Name:        s.stockName(tsCode),
			Qty:         pos.TotalQty,
			CostPrice:   pos.CostPrice,
			MarketPrice: price,
			FloatingPnL: pos.FloatingPnL,
			Suggestion:  suggestion,
		})
	}

	// 买入信号
	var buyList []TradeSuggestionJSON
	for _, sig := range signals {
		if sig.Direction != model.DirBuy {
			continue
		}

		price := 0.0
		if bar, ok := todayBars[sig.TsCode]; ok && bar.Close > 0 {
			price = bar.Close
		}
		if price <= 0 {
			continue
		}

		targetQty := sig.TargetQty
		if targetQty <= 0 {
			maxAmount := totalAsset * 0.2
			if maxAmount > 0 {
				targetQty = market.RoundLot(int(maxAmount / price))
			}
		}

		if targetQty <= 0 {
			continue
		}

		priority := int(10 - sig.Strength*9)
		if priority < 1 {
			priority = 1
		}

		buyList = append(buyList, TradeSuggestionJSON{
			TsCode:   sig.TsCode,
			Name:     s.stockName(sig.TsCode),
			Action:   "buy",
			DeltaQty: targetQty,
			Price:    price,
			Amount:   price * float64(targetQty),
			Priority: priority,
			Reason:   sig.Reason,
			Urgency:  "今日",
		})
	}

	// 建议现金比例
	cashPct := 0.15

	// 生成理由
	var reasonParts []string
	if len(sellList) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("建议卖出%d只", len(sellList)))
	}
	if len(buyList) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("建议买入%d只", len(buyList)))
	}
	if len(holdList) > 0 {
		reasonParts = append(reasonParts, fmt.Sprintf("持有%d只", len(holdList)))
	}
	if len(reasonParts) == 0 {
		reasonParts = append(reasonParts, "无明确调仓信号, 建议维持当前持仓")
	}
	reason := strings.Join(reasonParts, "; ")

	return &RebalanceJSON{
		SellList: sellList,
		BuyList:  buyList,
		HoldList: holdList,
		CashPct:  cashPct,
		Reason:   reason,
	}
}
