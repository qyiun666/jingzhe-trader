package risk

import (
	"fmt"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// PositionLimiter 仓位限制器
// 控制单票仓位、总仓位和板块仓位上限
type PositionLimiter struct {
	maxPositionPct      float64 // 单票最大仓位比例 (如 0.1 = 10%)
	maxTotalPositionPct float64 // 总仓位上限 (如 0.8 = 80%)
	maxSectorPct        float64 // 单板块上限 (如 0.3 = 30%)
}

// NewPositionLimiter 创建仓位限制器
// maxSingle: 单票最大仓位比例
// maxTotal: 总仓位上限比例
// maxSector: 单板块最大仓位比例
func NewPositionLimiter(maxSingle, maxTotal, maxSector float64) *PositionLimiter {
	return &PositionLimiter{
		maxPositionPct:      maxSingle,
		maxTotalPositionPct: maxTotal,
		maxSectorPct:        maxSector,
	}
}

// CheckPosition 检查买入信号是否突破仓位限制
// 返回调整后的信号（可能减少买入数量）和错误信息
// 对于卖出信号，直接返回原信号
func (pl *PositionLimiter) CheckPosition(signal model.Signal, positions map[string]*model.Position,
	totalAsset float64, stocks map[string]*model.Stock, price float64) (model.Signal, error) {

	// 卖出信号不做仓位限制检查
	if signal.Direction != model.DirBuy {
		return signal, nil
	}

	if price <= 0 || totalAsset <= 0 {
		return signal, fmt.Errorf("价格或总资产无效")
	}

	maxBuyQty := pl.CalcMaxBuyQty(signal.TsCode, positions, totalAsset, stocks, price)

	// 如果最大可买数量为 0，直接拒绝
	if maxBuyQty <= 0 {
		return signal, fmt.Errorf("仓位限制: 单票/总仓位/板块仓位已达上限，无法买入")
	}

	// 如果信号目标数量超过最大可买数量，调整为最大可买数量
	if signal.TargetQty > maxBuyQty {
		adjusted := signal
		adjusted.TargetQty = maxBuyQty
		adjusted.Reason = signal.Reason + " (仓位限制调整)"
		return adjusted, fmt.Errorf("仓位限制: 买入数量由 %d 调整为 %d", signal.TargetQty, maxBuyQty)
	}

	return signal, nil
}

// CalcMaxBuyQty 计算某股票最大可买数量
// 综合考虑单票仓位限制、总仓位限制和板块仓位限制
func (pl *PositionLimiter) CalcMaxBuyQty(tsCode string, positions map[string]*model.Position,
	totalAsset float64, stocks map[string]*model.Stock, price float64) int {

	if price <= 0 || totalAsset <= 0 {
		return 0
	}

	// 当前持仓
	currentPos := positions[tsCode]
	currentValue := 0.0
	if currentPos != nil {
		currentValue = float64(currentPos.TotalQty) * price
	}

	// 1. 单票仓位限制: 单票市值 <= 总资产 * maxPositionPct
	maxSingleValue := totalAsset * pl.maxPositionPct
	remainingSingleValue := maxSingleValue - currentValue
	if remainingSingleValue < 0 {
		remainingSingleValue = 0
	}
	maxQtyBySingle := int(remainingSingleValue / price)

	// 2. 总仓位限制: 所有持仓市值 <= 总资产 * maxTotalPositionPct
	totalMarketValue := 0.0
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		// 使用最新市价估算市值，如果没有则用成本价
		if pos.MarketPrice > 0 {
			totalMarketValue += pos.MarketValue
		} else {
			totalMarketValue += float64(pos.TotalQty) * pos.CostPrice
		}
	}
	maxTotalValue := totalAsset * pl.maxTotalPositionPct
	remainingTotalValue := maxTotalValue - totalMarketValue
	if remainingTotalValue < 0 {
		remainingTotalValue = 0
	}
	maxQtyByTotal := int(remainingTotalValue / price)

	// 3. 板块仓位限制
	maxQtyBySector := maxQtyBySingle // 默认取单票限制
	if pl.maxSectorPct > 0 {
		stock := stocks[tsCode]
		if stock != nil {
			board := model.DetectBoard(tsCode)
			sectorValue := 0.0
			for code, pos := range positions {
				if pos == nil {
					continue
				}
				b := model.DetectBoard(code)
				if b == board {
					if pos.MarketPrice > 0 {
						sectorValue += pos.MarketValue
					} else {
						sectorValue += float64(pos.TotalQty) * pos.CostPrice
					}
				}
			}
			maxSectorValue := totalAsset * pl.maxSectorPct
			remainingSectorValue := maxSectorValue - sectorValue
			if remainingSectorValue < 0 {
				remainingSectorValue = 0
			}
			maxQtyBySector = int(remainingSectorValue / price)
		}
	}

	// 取三者最小值
	maxQty := maxQtyBySingle
	if maxQtyByTotal < maxQty {
		maxQty = maxQtyByTotal
	}
	if maxQtyBySector < maxQty {
		maxQty = maxQtyBySector
	}

	// 手数取整（100 股的整数倍）
	maxQty = market.RoundLot(maxQty)

	if maxQty < 0 {
		return 0
	}
	return maxQty
}
