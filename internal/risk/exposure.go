package risk

import (
	"fmt"

	"jingzhe-trader/internal/model"
)

// ExposureManager 敞口与板块控制管理器
// 负责计算和控制各板块的风险敞口
type ExposureManager struct {
	maxSectorPct float64 // 单板块最大敞口比例 (如 0.3 = 30%)
}

// NewExposureManager 创建敞口管理器
// maxSectorPct: 单板块最大敞口比例
func NewExposureManager(maxSectorPct float64) *ExposureManager {
	return &ExposureManager{
		maxSectorPct: maxSectorPct,
	}
}

// SectorExposure 计算各板块敞口
// 返回各板块名称对应的敞口比例（相对于总资产）
func (em *ExposureManager) SectorExposure(positions map[string]*model.Position,
	stocks map[string]*model.Stock, totalAsset float64) map[string]float64 {

	exposure := make(map[string]float64)

	if totalAsset <= 0 {
		return exposure
	}

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		// 获取板块名称
		board := model.DetectBoard(tsCode)
		sectorName := boardName(board)

		// 计算市值
		marketValue := pos.MarketValue
		if marketValue <= 0 {
			if pos.MarketPrice > 0 {
				marketValue = float64(pos.TotalQty) * pos.MarketPrice
			} else if pos.CostPrice > 0 {
				marketValue = float64(pos.TotalQty) * pos.CostPrice
			}
		}

		exposure[sectorName] += marketValue / totalAsset
	}

	return exposure
}

// CheckSectorLimit 检查买入信号是否突破板块限制
// 买入后该板块总敞口不能超过 maxSectorPct
// buyQty: 拟买入数量
func (em *ExposureManager) CheckSectorLimit(signal model.Signal, positions map[string]*model.Position,
	stocks map[string]*model.Stock, totalAsset float64, price float64, buyQty int) error {

	if em.maxSectorPct <= 0 {
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

	// 获取信号股票的板块
	board := model.DetectBoard(signal.TsCode)
	sectorName := boardName(board)

	// 计算当前板块敞口
	currentSectorValue := 0.0
	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}
		b := model.DetectBoard(tsCode)
		if b == board {
			if pos.MarketValue > 0 {
				currentSectorValue += pos.MarketValue
			} else if pos.MarketPrice > 0 {
				currentSectorValue += float64(pos.TotalQty) * pos.MarketPrice
			} else if pos.CostPrice > 0 {
				currentSectorValue += float64(pos.TotalQty) * pos.CostPrice
			}
		}
	}

	// 拟买入金额
	buyValue := float64(buyQty) * price

	// 买入后的板块总市值
	newSectorValue := currentSectorValue + buyValue
	newSectorPct := newSectorValue / totalAsset

	if newSectorPct > em.maxSectorPct {
		return fmt.Errorf("板块敞口限制: %s 板块买入后敞口为 %.2f%%，超过上限 %.2f%%",
			sectorName, newSectorPct*100, em.maxSectorPct*100)
	}

	return nil
}

// boardName 返回板块的中文名称
func boardName(board model.Board) string {
	switch board {
	case model.BoardMainSH:
		return "沪市主板"
	case model.BoardMainSZ:
		return "深市主板"
	case model.BoardChiNext:
		return "创业板"
	case model.BoardSTAR:
		return "科创板"
	case model.BoardBSE:
		return "北交所"
	default:
		return "未知板块"
	}
}
