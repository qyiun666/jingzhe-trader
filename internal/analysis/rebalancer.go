package analysis

import (
	"fmt"
	"sort"
	"strings"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// RebalancePlan 调仓计划
type RebalancePlan struct {
	Date     string
	SellList []TradeSuggestion // 卖出清单 (优先级排序)
	BuyList  []TradeSuggestion // 买入清单 (优先级排序)
	HoldList []HoldSuggestion  // 持有清单 (不动)
	CashPct  float64           // 建议现金比例
	Reason   string            // 理由
}

// TradeSuggestion 交易建议
type TradeSuggestion struct {
	TsCode     string
	Name       string
	CurrentQty int
	TargetQty  int
	DeltaQty   int    // 正=买入, 负=卖出
	Price      float64
	Amount     float64
	Priority   int    // 1=最高优先级
	Reason     string // 如"止损-5%" / "策略信号买入" / "获利了结"
	Urgency    string // "立即" / "今日" / "观望"
}

// HoldSuggestion 持有建议
type HoldSuggestion struct {
	TsCode      string
	Name        string
	Qty         int
	CostPrice   float64
	MarketPrice float64
	FloatingPnL float64
	Suggestion  string // "继续持有" / "关注止损位" / "接近止盈"
}

// posDetail 持仓详情辅助结构
type posDetail struct {
	tsCode      string
	pos         *model.Position
	price       float64
	marketValue float64
	weight      float64
}

// GenerateRebalancePlan 生成调仓计划
// currentPositions: 当前持仓
// strategySignals: 策略产生的信号
// portfolioAnalysis: 持仓分析结果
// riskConfig: 风控配置
func GenerateRebalancePlan(
	tradeDate string,
	currentPositions map[string]*model.Position,
	strategySignals []model.Signal,
	portfolioAnalysis *PortfolioAnalysis,
	bars map[string]*model.Bar,
	totalAsset float64,
	riskConfig config.RiskConfig,
) *RebalancePlan {
	plan := &RebalancePlan{
		Date: tradeDate,
	}

	// 准备持仓详情并按市值排序
	posDetails := buildPosDetails(currentPositions, bars, totalAsset)

	// 建立信号索引
	signalMap := make(map[string]model.Signal)
	for _, sig := range strategySignals {
		signalMap[sig.TsCode] = sig
	}

	// 确定前5大持仓集合 (用于集中度判断)
	top5Set := make(map[string]bool)
	for i := 0; i < 5 && i < len(posDetails); i++ {
		top5Set[posDetails[i].tsCode] = true
	}

	// 1. 遍历当前持仓,决定卖出或持有
	for _, pd := range posDetails {
		pos := pd.pos
		sig, hasSignal := signalMap[pd.tsCode]

		// 1.1 止损检查 (优先级1)
		if riskConfig.StopLossPct > 0 && pos.FloatingPnLPct < -riskConfig.StopLossPct {
			sellQty := pos.AvailableQty
			if sellQty > 0 {
				plan.SellList = append(plan.SellList, TradeSuggestion{
					TsCode:     pd.tsCode,
					CurrentQty: pos.TotalQty,
					TargetQty:  pos.TotalQty - sellQty,
					DeltaQty:   -sellQty,
					Price:      pd.price,
					Amount:     pd.price * float64(sellQty),
					Priority:   1,
					Reason:     fmt.Sprintf("止损触发,浮亏%.1f%%", pos.FloatingPnLPct*100),
					Urgency:    "立即",
				})
			}
			continue
		}

		// 1.2 止盈检查 (优先级2)
		if riskConfig.TakeProfitPct > 0 && pos.FloatingPnLPct > riskConfig.TakeProfitPct {
			sellQty := pos.AvailableQty
			if sellQty > 0 {
				plan.SellList = append(plan.SellList, TradeSuggestion{
					TsCode:     pd.tsCode,
					CurrentQty: pos.TotalQty,
					TargetQty:  pos.TotalQty - sellQty,
					DeltaQty:   -sellQty,
					Price:      pd.price,
					Amount:     pd.price * float64(sellQty),
					Priority:   2,
					Reason:     fmt.Sprintf("止盈触发,浮盈%.1f%%", pos.FloatingPnLPct*100),
					Urgency:    "今日",
				})
			}
			continue
		}

		// 1.3 策略信号卖出 (优先级3)
		if hasSignal && sig.Direction == model.DirSell {
			sellQty := pos.AvailableQty
			if sig.TargetQty > 0 && sig.TargetQty < sellQty {
				sellQty = sig.TargetQty
			}
			if sellQty > 0 {
				plan.SellList = append(plan.SellList, TradeSuggestion{
					TsCode:     pd.tsCode,
					CurrentQty: pos.TotalQty,
					TargetQty:  pos.TotalQty - sellQty,
					DeltaQty:   -sellQty,
					Price:      pd.price,
					Amount:     pd.price * float64(sellQty),
					Priority:   3,
					Reason:     "策略信号: " + sig.Reason,
					Urgency:    "今日",
				})
			}
			continue
		}

		// 1.4 集中度太高且不在买入信号中 (优先级4,减仓)
		if portfolioAnalysis != nil && portfolioAnalysis.Concentration.Top5Pct > 0.6 &&
			top5Set[pd.tsCode] && (!hasSignal || sig.Direction != model.DirBuy) {
			sellQty := pos.AvailableQty / 2
			if sellQty > 0 {
				plan.SellList = append(plan.SellList, TradeSuggestion{
					TsCode:     pd.tsCode,
					CurrentQty: pos.TotalQty,
					TargetQty:  pos.TotalQty - sellQty,
					DeltaQty:   -sellQty,
					Price:      pd.price,
					Amount:     pd.price * float64(sellQty),
					Priority:   4,
					Reason:     fmt.Sprintf("集中度太高(TOP5:%.1f%%),减仓", portfolioAnalysis.Concentration.Top5Pct*100),
					Urgency:    "观望",
				})
			}
			continue
		}

		// 1.5 继续持有
		suggestion := "继续持有"
		if pos.FloatingPnLPct < -0.03 {
			suggestion = "关注止损位"
		} else if riskConfig.TakeProfitPct > 0 && pos.FloatingPnLPct > riskConfig.TakeProfitPct*0.8 {
			suggestion = "接近止盈"
		}
		plan.HoldList = append(plan.HoldList, HoldSuggestion{
			TsCode:      pd.tsCode,
			Qty:         pos.TotalQty,
			CostPrice:   pos.CostPrice,
			MarketPrice: pd.price,
			FloatingPnL: pos.FloatingPnL,
			Suggestion:  suggestion,
		})
	}

	// 2. 遍历策略买入信号
	for _, sig := range strategySignals {
		if sig.Direction != model.DirBuy {
			continue
		}

		price := 0.0
		if bar := bars[sig.TsCode]; bar != nil && bar.Close > 0 {
			price = bar.Close
		}
		if price <= 0 {
			continue
		}

		pos, hasPos := currentPositions[sig.TsCode]

		// 计算目标数量
		targetQty := sig.TargetQty
		if targetQty <= 0 {
			// 默认按单票最大仓位比例分配
			maxAmount := totalAsset * riskConfig.MaxPositionPct
			if maxAmount > 0 {
				targetQty = market.RoundLot(int(maxAmount / price))
			}
		}

		if hasPos && pos != nil {
			if pos.TotalQty >= targetQty {
				continue // 持仓已足够,无需加仓
			}
			targetQty = targetQty - pos.TotalQty
		}

		if targetQty <= 0 {
			continue
		}

		// 检查单票仓位限制
		var currentMV float64
		if hasPos && pos != nil {
			currentMV = float64(pos.TotalQty) * price
		}
		finalMV := currentMV + float64(targetQty)*price
		if totalAsset > 0 && finalMV/totalAsset > riskConfig.MaxPositionPct {
			maxMV := totalAsset * riskConfig.MaxPositionPct
			allowMV := maxMV - currentMV
			if allowMV > 0 {
				targetQty = market.RoundLot(int(allowMV / price))
			} else {
				continue
			}
		}

		if targetQty <= 0 {
			continue
		}

		// 优先级: 信号强度越高,优先级越靠前 (Priority 越小越优先)
		priority := int(10 - sig.Strength*9)
		if priority < 1 {
			priority = 1
		}

		plan.BuyList = append(plan.BuyList, TradeSuggestion{
			TsCode:     sig.TsCode,
			CurrentQty: 0,
			TargetQty:  targetQty,
			DeltaQty:   targetQty,
			Price:      price,
			Amount:     price * float64(targetQty),
			Priority:   priority,
			Reason:     sig.Reason,
			Urgency:    "今日",
		})
	}

	// 按优先级排序
	sort.Slice(plan.SellList, func(i, j int) bool {
		return plan.SellList[i].Priority < plan.SellList[j].Priority
	})
	sort.Slice(plan.BuyList, func(i, j int) bool {
		return plan.BuyList[i].Priority < plan.BuyList[j].Priority
	})

	// 3. 确定建议现金比例
	plan.CashPct = determineCashPct(portfolioAnalysis, bars)

	// 4. 生成理由
	plan.Reason = buildRebalanceReason(plan)

	return plan
}

// buildPosDetails 构建持仓详情列表并按市值降序排列
func buildPosDetails(
	positions map[string]*model.Position,
	bars map[string]*model.Bar,
	totalAsset float64,
) []posDetail {
	var details []posDetail
	var totalMV float64

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		price := 0.0
		if bar := bars[tsCode]; bar != nil && bar.Close > 0 {
			price = bar.Close
		} else if pos.MarketPrice > 0 {
			price = pos.MarketPrice
		}
		if price <= 0 {
			continue
		}

		mv := float64(pos.TotalQty) * price
		details = append(details, posDetail{
			tsCode:      tsCode,
			pos:         pos,
			price:       price,
			marketValue: mv,
		})
		totalMV += mv
	}

	// 按市值降序排序
	sort.Slice(details, func(i, j int) bool {
		return details[i].marketValue > details[j].marketValue
	})

	// 计算权重
	for i := range details {
		if totalAsset > 0 {
			details[i].weight = details[i].marketValue / totalAsset
		}
	}

	return details
}

// determineCashPct 根据市场环境确定建议现金比例
func determineCashPct(pa *PortfolioAnalysis, bars map[string]*model.Bar) float64 {
	// 尝试获取指数当日行情辅助判断
	marketBar := bars["000300.SH"]
	if marketBar == nil {
		marketBar = bars["000001.SH"]
	}
	if marketBar == nil {
		marketBar = bars["399001.SZ"]
	}

	badMarket := false
	if pa != nil {
		// 健康度低 -> 市场环境差
		if pa.HealthScore < 50 {
			badMarket = true
		}
		// 胜率低且总体亏损
		if pa.PnLAttribution.WinPct < 0.35 && pa.PnLAttribution.TotalFloatingPnL < 0 {
			badMarket = true
		}
		// 大幅亏损且亏损股多于盈利股
		if pa.RiskMetrics.MaxSingleLossPct < -0.1 && pa.PnLAttribution.LossCount > pa.PnLAttribution.WinCount {
			badMarket = true
		}
	}

	// 指数当日大跌
	if marketBar != nil && marketBar.PctChg < -1.5 {
		badMarket = true
	}

	if badMarket {
		if pa != nil && pa.HealthScore < 30 {
			return 0.5 // 极度危险,保持50%现金
		}
		return 0.35 // 市场环境差,保持35%现金
	}

	// 正常市场环境,保持10-20%现金,根据VaR动态调整
	cashPct := 0.15
	if pa != nil && pa.RiskMetrics.VaR95 > 0.02 {
		cashPct = 0.2
	}
	return cashPct
}

// buildRebalanceReason 构建调仓理由
func buildRebalanceReason(plan *RebalancePlan) string {
	var parts []string
	if len(plan.SellList) > 0 {
		parts = append(parts, fmt.Sprintf("建议卖出%d只", len(plan.SellList)))
	}
	if len(plan.BuyList) > 0 {
		parts = append(parts, fmt.Sprintf("建议买入%d只", len(plan.BuyList)))
	}
	if len(plan.HoldList) > 0 {
		parts = append(parts, fmt.Sprintf("持有%d只", len(plan.HoldList)))
	}
	parts = append(parts, fmt.Sprintf("建议现金比例:%.0f%%", plan.CashPct*100))

	// 补充卖出原因摘要
	var stopLossCount, takeProfitCount, signalSellCount int
	for _, s := range plan.SellList {
		if s.Priority == 1 {
			stopLossCount++
		} else if s.Priority == 2 {
			takeProfitCount++
		} else if s.Priority == 3 {
			signalSellCount++
		}
	}
	if stopLossCount > 0 {
		parts = append(parts, fmt.Sprintf("止损%d只", stopLossCount))
	}
	if takeProfitCount > 0 {
		parts = append(parts, fmt.Sprintf("止盈%d只", takeProfitCount))
	}
	if signalSellCount > 0 {
		parts = append(parts, fmt.Sprintf("策略卖出%d只", signalSellCount))
	}

	return strings.Join(parts, "; ")
}
