package portfolio

import (
	"fmt"
	"sort"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// TargetPosition 目标持仓
type TargetPosition struct {
	TsCode    string
	TargetQty int     // 目标数量
	Weight    float64 // 目标权重 (占总资产比例)
	Reason    string  // 调仓原因
}

// RebalanceOrder 调仓指令
type RebalanceOrder struct {
	TsCode    string
	Side      model.Side
	Qty       int
	Reason    string
}

// Manager 组合管理器
// 负责目标持仓管理、调仓差分、资金分配
type Manager struct {
	targets map[string]*TargetPosition // 目标持仓
}

// NewManager 创建组合管理器
func NewManager() *Manager {
	return &Manager{
		targets: make(map[string]*TargetPosition),
	}
}

// SetTargets 设置目标持仓
func (pm *Manager) SetTargets(targets []TargetPosition) {
	pm.targets = make(map[string]*TargetPosition, len(targets))
	for i := range targets {
		pm.targets[targets[i].TsCode] = &targets[i]
	}
}

// GetTarget 获取某股票的目标持仓
func (pm *Manager) GetTarget(tsCode string) *TargetPosition {
	return pm.targets[tsCode]
}

// Rebalance 计算调仓指令
// 对比目标持仓和当前持仓, 生成买卖指令
func (pm *Manager) Rebalance(positions map[string]*model.Position, prices map[string]float64) []RebalanceOrder {
	var orders []RebalanceOrder

	// 1. 处理需要卖出的股票 (当前持仓但不在目标中, 或目标数量减少)
	for tsCode, pos := range positions {
		target, hasTarget := pm.targets[tsCode]
		if !hasTarget {
			// 清仓
			if pos.AvailableQty > 0 {
				orders = append(orders, RebalanceOrder{
					TsCode: tsCode,
					Side:   model.SideSell,
					Qty:    pos.AvailableQty,
					Reason: "不在目标持仓中, 清仓",
				})
			}
		} else if pos.TotalQty > target.TargetQty {
			// 减仓
			reduceQty := pos.TotalQty - target.TargetQty
			if reduceQty > pos.AvailableQty {
				reduceQty = pos.AvailableQty
			}
			if reduceQty > 0 {
				orders = append(orders, RebalanceOrder{
					TsCode: tsCode,
					Side:   model.SideSell,
					Qty:    reduceQty,
					Reason: fmt.Sprintf("减仓: 当前%d -> 目标%d", pos.TotalQty, target.TargetQty),
				})
			}
		}
	}

	// 2. 处理需要买入的股票 (目标中但不在当前持仓, 或目标数量增加)
	for tsCode, target := range pm.targets {
		pos, hasPos := positions[tsCode]
		if !hasPos {
			// 新建仓
			if target.TargetQty > 0 {
				orders = append(orders, RebalanceOrder{
					TsCode: tsCode,
					Side:   model.SideBuy,
					Qty:    target.TargetQty,
					Reason: target.Reason,
				})
			}
		} else if target.TargetQty > pos.TotalQty {
			// 加仓
			addQty := target.TargetQty - pos.TotalQty
			orders = append(orders, RebalanceOrder{
				TsCode: tsCode,
				Side:   model.SideBuy,
				Qty:    addQty,
				Reason: fmt.Sprintf("加仓: 当前%d -> 目标%d", pos.TotalQty, target.TargetQty),
			})
		}
	}

	return orders
}

// AllocateEqual 等权重分配资金
// 将 totalAsset 均分给 n 只股票, 计算每只股票的目标数量
func AllocateEqual(tsCodes []string, totalAsset float64, prices map[string]float64) []TargetPosition {
	if len(tsCodes) == 0 || totalAsset <= 0 {
		return nil
	}
	perStock := totalAsset / float64(len(tsCodes))
	weight := 1.0 / float64(len(tsCodes))
	result := make([]TargetPosition, 0, len(tsCodes))
	for _, tsCode := range tsCodes {
		price, ok := prices[tsCode]
		if !ok || price <= 0 {
			continue
		}
		qty := market.RoundLot(int(perStock / price))
		if qty > 0 {
			result = append(result, TargetPosition{
				TsCode:    tsCode,
				TargetQty: qty,
				Weight:    weight,
				Reason:    "等权重分配",
			})
		}
	}
	return result
}

// AllocateByScore 按得分比例分配资金
// scores: map[tsCode]score (得分越高, 分配越多)
func AllocateByScore(scores map[string]float64, totalAsset float64, prices map[string]float64) []TargetPosition {
	if len(scores) == 0 || totalAsset <= 0 {
		return nil
	}

	// 计算总分
	var totalScore float64
	for _, s := range scores {
		if s > 0 {
			totalScore += s
		}
	}
	if totalScore <= 0 {
		return nil
	}

	result := make([]TargetPosition, 0, len(scores))
	for tsCode, score := range scores {
		if score <= 0 {
			continue
		}
		price, ok := prices[tsCode]
		if !ok || price <= 0 {
			continue
		}
		weight := score / totalScore
		amount := totalAsset * weight
		qty := market.RoundLot(int(amount / price))
		if qty > 0 {
			result = append(result, TargetPosition{
				TsCode:    tsCode,
				TargetQty: qty,
				Weight:    weight,
				Reason:    fmt.Sprintf("按得分分配: %.2f", score),
			})
		}
	}

	// 按得分降序排列
	sort.Slice(result, func(i, j int) bool {
		return scores[result[i].TsCode] > scores[result[j].TsCode]
	})

	return result
}

// TotalTargetValue 目标持仓总市值
func (pm *Manager) TotalTargetValue(prices map[string]float64) float64 {
	total := 0.0
	for tsCode, target := range pm.targets {
		if price, ok := prices[tsCode]; ok {
			total += price * float64(target.TargetQty)
		}
	}
	return total
}

// Turnover 目标换手率
// 计算当前持仓与目标持仓的差异占总资产的比例
func (pm *Manager) Turnover(positions map[string]*model.Position, prices map[string]float64, totalAsset float64) float64 {
	if totalAsset <= 0 {
		return 0
	}
	var diffValue float64

	// 卖出部分
	for tsCode, pos := range positions {
		price := prices[tsCode]
		if price <= 0 {
			continue
		}
		target, hasTarget := pm.targets[tsCode]
		if !hasTarget {
			diffValue += price * float64(pos.AvailableQty)
		} else if pos.TotalQty > target.TargetQty {
			diffValue += price * float64(pos.TotalQty-target.TargetQty)
		}
	}

	// 买入部分
	for tsCode, target := range pm.targets {
		price := prices[tsCode]
		if price <= 0 {
			continue
		}
		pos, hasPos := positions[tsCode]
		if !hasPos {
			diffValue += price * float64(target.TargetQty)
		} else if target.TargetQty > pos.TotalQty {
			diffValue += price * float64(target.TargetQty-pos.TotalQty)
		}
	}

	return diffValue / totalAsset
}
