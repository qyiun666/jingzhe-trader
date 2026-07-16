package risk

import (
	"fmt"

	"jingzhe-trader/internal/model"
)

// StopLossManager 止损止盈管理器
// 监控持仓的盈亏情况，触发止损或止盈时生成卖出信号
type StopLossManager struct {
	stopLossPct   float64 // 止损比例 (如 0.05 = 5%)
	takeProfitPct float64 // 止盈比例 (如 0.15 = 15%)
	trailingStop  float64 // 移动止盈回撤比例 (可选，0 表示不启用)
}

// NewStopLossManager 创建止损止盈管理器
// stopLossPct: 止损比例，相对于成本价的跌幅
// takeProfitPct: 止盈比例，相对于成本价的涨幅
func NewStopLossManager(stopLossPct, takeProfitPct float64) *StopLossManager {
	return &StopLossManager{
		stopLossPct:   stopLossPct,
		takeProfitPct: takeProfitPct,
		trailingStop:  0,
	}
}

// SetTrailingStop 设置移动止盈回撤比例
// 当盈利达到 takeProfitPct 后启用，价格从高点回撤 trailingStop 比例时触发止盈
func (sl *StopLossManager) SetTrailingStop(trailingPct float64) {
	sl.trailingStop = trailingPct
}

// CheckStopLoss 检查所有持仓是否触发止损/止盈
// 返回需要卖出的信号列表
// bars: 各股票最新K线数据，用于获取当前价格
func (sl *StopLossManager) CheckStopLoss(positions map[string]*model.Position,
	bars map[string]*model.Bar) []model.Signal {

	var signals []model.Signal

	for tsCode, pos := range positions {
		if pos == nil || pos.TotalQty <= 0 {
			continue
		}

		bar := bars[tsCode]
		if bar == nil {
			continue
		}

		currentPrice := bar.Close
		if currentPrice <= 0 {
			continue
		}

		triggered, reason := sl.CheckSingle(pos, currentPrice)
		if triggered {
			signals = append(signals, model.Signal{
				TsCode:    tsCode,
				Direction: model.DirSell,
				TargetQty: pos.AvailableQty,
				Reason:    reason,
				Strength:  1.0,
			})
		}
	}

	return signals
}

// CheckSingle 检查单只持仓是否触发止损或止盈
// 返回是否触发和触发原因
func (sl *StopLossManager) CheckSingle(pos *model.Position, currentPrice float64) (bool, string) {
	if pos == nil || pos.TotalQty <= 0 || pos.CostPrice <= 0 || currentPrice <= 0 {
		return false, ""
	}

	// 计算盈亏比例 (相对于成本价)
	pnlPct := (currentPrice - pos.CostPrice) / pos.CostPrice

	// 检查止损
	if sl.stopLossPct > 0 && pnlPct <= -sl.stopLossPct {
		return true, fmt.Sprintf("止损触发: 亏损 %.2f%% (成本 %.2f, 现价 %.2f)",
			pnlPct*100, pos.CostPrice, currentPrice)
	}

	// 检查止盈
	if sl.takeProfitPct > 0 && pnlPct >= sl.takeProfitPct {
		// 如果启用了移动止盈，使用移动止盈逻辑
		if sl.trailingStop > 0 {
			// 从最高点回撤超过 trailingStop 才触发
			highPrice := pos.MarketPrice
			if highPrice < currentPrice {
				highPrice = currentPrice
			}
			if highPrice > pos.CostPrice {
				drawdown := (highPrice - currentPrice) / highPrice
				if drawdown >= sl.trailingStop {
					return true, fmt.Sprintf("移动止盈触发: 从高点回撤 %.2f%% (高点 %.2f, 现价 %.2f)",
						drawdown*100, highPrice, currentPrice)
				}
				// 未达到回撤阈值，继续持有
				return false, ""
			}
		}
		return true, fmt.Sprintf("止盈触发: 盈利 %.2f%% (成本 %.2f, 现价 %.2f)",
			pnlPct*100, pos.CostPrice, currentPrice)
	}

	return false, ""
}
