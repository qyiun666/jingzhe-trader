package risk

import (
	"fmt"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// PositionLimiter 仓位限制器
// 控制单票仓位、总仓位和板块敞口上限
// 板块口径统一为 sectorOf: 真实行业 (stock_basic.industry) 优先, 缺失回退交易所板块
type PositionLimiter struct {
	maxPositionPct      float64 // 单票最大仓位比例 (如 0.1 = 10%)
	maxTotalPositionPct float64 // 总仓位上限 (如 0.8 = 80%)
	maxSectorPct        float64 // 单板块敞口上限 (如 0.3 = 30%)
}

// NewPositionLimiter 创建仓位限制器
// maxSingle: 单票最大仓位比例
// maxTotal: 总仓位上限比例
// maxSector: 单板块最大敞口比例
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
		return signal, fmt.Errorf("仓位限制: 单票/总仓位/板块敞口已达上限，无法买入")
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
// 综合考虑单票仓位限制、总仓位限制和板块敞口限制
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
		totalMarketValue += holdingValue(pos)
	}
	maxTotalValue := totalAsset * pl.maxTotalPositionPct
	remainingTotalValue := maxTotalValue - totalMarketValue
	if remainingTotalValue < 0 {
		remainingTotalValue = 0
	}
	maxQtyByTotal := int(remainingTotalValue / price)

	// 3. 板块敞口限制 (与 CheckSectorLimit 同一口径: 行业优先, 回退交易所板块)
	maxQtyBySector := maxQtyBySingle // 默认取单票限制
	if pl.maxSectorPct > 0 {
		sectorName := sectorOf(stocks[tsCode], tsCode)
		sectorValue := 0.0
		for code, pos := range positions {
			if pos == nil {
				continue
			}
			if sectorOf(stocks[code], code) == sectorName {
				sectorValue += holdingValue(pos)
			}
		}
		maxSectorValue := totalAsset * pl.maxSectorPct
		remainingSectorValue := maxSectorValue - sectorValue
		if remainingSectorValue < 0 {
			remainingSectorValue = 0
		}
		maxQtyBySector = int(remainingSectorValue / price)
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

// SectorExposure 计算各板块敞口
// 返回各板块名称对应的敞口比例（相对于总资产）
func (pl *PositionLimiter) SectorExposure(positions map[string]*model.Position,
	stocks map[string]*model.Stock, totalAsset float64) map[string]float64 {

	exposure := make(map[string]float64)

	if totalAsset <= 0 {
		return exposure
	}

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		// 敞口分类: 优先行业 (stock_basic.industry), 缺失时按交易所板块兜底
		sectorName := sectorOf(stocks[tsCode], tsCode)

		// 计算市值 (统一回退链: 市值 → 市价×数量 → 成本价×数量)
		exposure[sectorName] += holdingValue(pos) / totalAsset
	}

	return exposure
}

// CheckSectorLimit 检查买入信号是否突破板块敞口限制
// 买入后该板块总敞口不能超过 maxSectorPct
// buyQty: 拟买入数量
func (pl *PositionLimiter) CheckSectorLimit(signal model.Signal, positions map[string]*model.Position,
	stocks map[string]*model.Stock, totalAsset float64, price float64, buyQty int) error {

	if pl.maxSectorPct <= 0 {
		// 未设置板块限制，直接通过
		return nil
	}

	if signal.Direction != model.DirBuy {
		// 卖出信号不检查板块限制
		return nil
	}

	if totalAsset <= 0 || price <= 0 || buyQty <= 0 {
		return fmt.Errorf("参数无效: 总资产或价格或买入数量不合法")
	}

	// 获取信号股票的行业 (缺失时回退交易所板块)
	sectorName := sectorOf(stocks[signal.TsCode], signal.TsCode)

	// 计算当前行业敞口
	currentSectorValue := 0.0
	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}
		if sectorOf(stocks[tsCode], tsCode) == sectorName {
			currentSectorValue += holdingValue(pos)
		}
	}

	// 拟买入金额
	buyValue := float64(buyQty) * price

	// 买入后的板块总市值
	newSectorValue := currentSectorValue + buyValue
	newSectorPct := newSectorValue / totalAsset

	if newSectorPct > pl.maxSectorPct {
		return fmt.Errorf("板块敞口限制: %s 板块买入后敞口为 %.2f%%，超过上限 %.2f%%",
			sectorName, newSectorPct*100, pl.maxSectorPct*100)
	}

	return nil
}

// sectorOf 返回股票所属行业; 行业信息缺失时回退到交易所板块
func sectorOf(stock *model.Stock, tsCode string) string {
	if stock != nil && stock.Industry != "" {
		return stock.Industry
	}
	return model.MarketFromCode(tsCode)
}

// holdingValue 计算持仓市值: 优先 MarketValue, 其次 数量×市价, 最后 数量×成本价
func holdingValue(pos *model.Position) float64 {
	if pos == nil {
		return 0
	}
	if pos.MarketValue > 0 {
		return pos.MarketValue
	}
	if pos.MarketPrice > 0 {
		return float64(pos.TotalQty) * pos.MarketPrice
	}
	if pos.CostPrice > 0 {
		return float64(pos.TotalQty) * pos.CostPrice
	}
	return 0
}
