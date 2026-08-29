package risk

import (
	"fmt"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// RejectReason 拒绝原因
type RejectReason struct {
	TsCode string       // 股票代码
	Signal model.Signal // 原始信号
	Reason string       // 拒绝原因描述
	Rule   string       // 触发的规则名
}

// reject 构造拒绝原因 (统一 13 处构造样板)
func reject(sig model.Signal, rule, reason string) *RejectReason {
	return &RejectReason{TsCode: sig.TsCode, Signal: sig, Reason: reason, Rule: rule}
}

// RiskManager 风控管理器
// 所有信号进入执行前都必须经过风控检查
// 检查顺序: 黑名单 -> 仓位限制(含板块敞口) -> 止损止盈 -> 小资金 -> T+1
type RiskManager struct {
	cfg             config.RiskConfig
	positionLimiter *PositionLimiter
	stopLossManager *StopLossManager
	blacklist       *Blacklist
	sizeLimits      SizeLimits // 小资金资金管理 (最小单笔金额/最大持仓数)
}

// NewRiskManager 创建风控管理器
func NewRiskManager(cfg config.RiskConfig) *RiskManager {
	sl := NewStopLossManager(cfg.StopLossPct, cfg.TakeProfitPct)
	if cfg.TrailingStopPct > 0 {
		sl.SetTrailingStop(cfg.TrailingStopPct)
	}
	return &RiskManager{
		cfg:             cfg,
		positionLimiter: NewPositionLimiter(cfg.MaxPositionPct, cfg.MaxTotalPositionPct, cfg.MaxSectorPct),
		stopLossManager: sl,
		blacklist:       NewBlacklist(cfg.ExcludeST, cfg.MinListDays),
	}
}

// PositionLimiter 获取仓位限制器
func (rm *RiskManager) PositionLimiter() *PositionLimiter {
	return rm.positionLimiter
}

// StopLossManager 获取止损止盈管理器
func (rm *RiskManager) StopLossManager() *StopLossManager {
	return rm.stopLossManager
}

// Blacklist 获取黑名单
func (rm *RiskManager) Blacklist() *Blacklist {
	return rm.blacklist
}

// SetSizeLimits 配置小资金资金管理限制
func (rm *RiskManager) SetSizeLimits(limits SizeLimits) {
	rm.sizeLimits = limits
}

// checkSizeLimits 小资金资金管理检查 (仅针对买入信号)
//  1. 单笔金额低于最小交易额时, 先尝试逐手上调数量达标 (整手取整误差补偿),
//     上调后仍超单票仓位上限则拒绝 (避免最低佣金侵蚀)
//  2. 新开仓超过最大持仓数时拒绝
func (rm *RiskManager) checkSizeLimits(sig model.Signal, currentPrice, totalAsset float64,
	positions map[string]*model.Position, newCodes map[string]bool) (model.Signal, *RejectReason) {

	if minAmount := rm.sizeLimits.ResolveMinAmount(); minAmount > 0 && currentPrice > 0 {
		amount := currentPrice * float64(sig.TargetQty)
		if amount < minAmount {
			// 整手取整导致的小额缺口: 逐手上调, 不突破单票仓位上限
			maxAmount := totalAsset * rm.cfg.MaxPositionPct
			qty := sig.TargetQty
			for currentPrice*float64(qty) < minAmount &&
				currentPrice*float64(qty+100) <= maxAmount {
				qty += 100
			}
			if currentPrice*float64(qty) < minAmount {
				return sig, reject(sig, "min_trade_amount", fmt.Sprintf("单笔金额 %.0f 低于最小交易额 %.0f (最低佣金侵蚀)", amount, minAmount))
			}
			sig.TargetQty = qty
		}
	}

	if maxPos := rm.sizeLimits.ResolveMaxPositions(totalAsset); maxPos > 0 {
		pos := positions[sig.TsCode]
		isNew := (pos == nil || pos.TotalQty <= 0) && !newCodes[sig.TsCode]
		if isNew {
			held := len(newCodes)
			for _, p := range positions {
				if p != nil && p.TotalQty > 0 {
					held++
				}
			}
			if held >= maxPos {
				return sig, reject(sig, "max_positions", fmt.Sprintf("持仓数已达上限 %d (小资金集中持仓)", maxPos))
			}
		}
	}
	return sig, nil
}

// Check 检查信号，返回通过的信号和被拒绝的原因
// 检查顺序: 黑名单 -> 仓位限制 -> 止损止盈 -> 敞口控制 -> T+1 -> 涨跌停
//
// 参数:
//   - signals: 待检查的交易信号列表
//   - positions: 当前持仓映射
//   - totalAsset: 总资产
//   - stocks: 股票基本信息映射
//   - tradeDate: 当前交易日期 YYYYMMDD
//   - bars: 当日K线数据（用于获取当前价格、涨跌停价等）
//
// 返回:
//   - 通过风控检查的信号列表（买入信号可能已调整数量）
//   - 被拒绝的原因列表
func (rm *RiskManager) Check(signals []model.Signal, positions map[string]*model.Position,
	totalAsset float64, stocks map[string]*model.Stock, tradeDate string,
	bars map[string]*model.Bar) ([]model.Signal, []RejectReason) {

	var passed []model.Signal
	var rejected []RejectReason
	// 本批次已通过的新开仓代码 (用于最大持仓数检查)
	newCodes := make(map[string]bool)
	// 本批次在途买入市值累计 (防同批次多笔买入叠加绕过总仓位/板块敞口约束)
	batch := newBatchPending()

	// 第一步：黑名单过滤
	survived, blRejected := rm.blacklist.FilterSignals(signals, stocks, tradeDate)
	rejected = append(rejected, blRejected...)

	// 第二步：逐个检查剩余信号
	for _, sig := range survived {
		// 获取当前价格
		currentPrice := 0.0
		bar := bars[sig.TsCode]
		if bar != nil {
			currentPrice = bar.Close
		}
		// 如果K线没有价格，尝试用持仓的市价或成本价
		if currentPrice <= 0 {
			if pos := positions[sig.TsCode]; pos != nil {
				if pos.MarketPrice > 0 {
					currentPrice = pos.MarketPrice
				} else if pos.CostPrice > 0 {
					currentPrice = pos.CostPrice
				}
			}
		}

		// 获取涨跌停价
		upLimit := 0.0
		downLimit := 0.0
		if bar != nil && bar.PreClose > 0 {
			stock := stocks[sig.TsCode]
			isST := false
			if stock != nil {
				isST = stock.IsST
			}
			// 使用市场规则计算涨跌停价
			// 这里简单估算，实际应使用 StkLimit 数据
			upLimit = market.CalcUpLimit(bar.PreClose, sig.TsCode, isST, bar.Date())
			downLimit = market.CalcDownLimit(bar.PreClose, sig.TsCode, isST, bar.Date())
		}

		// 根据方向分别检查
		if sig.Direction == model.DirBuy {
			// 买入信号检查

			// 1. 涨跌停检查：涨停不能买入
			if currentPrice > 0 && upLimit > 0 {
				if err := market.CheckLimit(model.SideBuy, currentPrice, upLimit, downLimit); err != nil {
					rejected = append(rejected, *reject(sig, "limit_up_buy", err.Error()))
					continue
				}
			}

			// 2. 仓位限制检查（可能调整买入数量, 含本批次在途买入）
			adjusted, err := rm.positionLimiter.checkPosition(sig, positions, totalAsset, stocks, currentPrice, batch)
			if err != nil {
				// 如果调整后数量为 0，完全拒绝
				if adjusted.TargetQty <= 0 {
					rejected = append(rejected, *reject(sig, "position_limit", err.Error()))
					continue
				}
				// 部分调整，继续后续检查
				sig = adjusted
			}

			// 3. 小资金资金管理检查 (最小单笔金额/最大持仓数); 可能上调数量补偿取整误差
			sized, rej := rm.checkSizeLimits(sig, currentPrice, totalAsset, positions, newCodes)
			if rej != nil {
				rejected = append(rejected, *rej)
				continue
			}
			sig = sized

			// 4. 板块敞口控制检查（板块限制, 含本批次在途买入）
			if err := rm.positionLimiter.checkSectorLimit(sig, positions, stocks, totalAsset, currentPrice, sig.TargetQty, batch); err != nil {
				rejected = append(rejected, *reject(sig, "sector_exposure", err.Error()))
				continue
			}

			if pos := positions[sig.TsCode]; pos == nil || pos.TotalQty <= 0 {
				newCodes[sig.TsCode] = true
			}
			// 记录本批次在途买入市值, 供后续信号的同批次累计约束
			batch.add(sig.TsCode, sectorOf(stocks[sig.TsCode], sig.TsCode), float64(sig.TargetQty)*currentPrice)
			passed = append(passed, sig)

		} else if sig.Direction == model.DirSell {
			// 卖出信号检查

			pos := positions[sig.TsCode]

			// 1. 检查是否有持仓
			if pos == nil || pos.TotalQty <= 0 {
				rejected = append(rejected, *reject(sig, "no_position", "无持仓可卖"))
				continue
			}

			// 2. T+1 可卖检查
			if !market.CanSell(pos, sig.TargetQty) {
				// 如果可卖量不足，调整为可卖数量
				if pos.AvailableQty > 0 {
					adjusted := sig
					adjusted.TargetQty = pos.AvailableQty
					adjusted.Reason = sig.Reason + fmt.Sprintf(" (T+1调整: 可卖%d股)", pos.AvailableQty)
					sig = adjusted
				} else {
					rejected = append(rejected, *reject(sig, "t1_restriction", fmt.Sprintf("T+1限制: 可卖量不足(可卖%d, 需卖%d)", pos.AvailableQty, sig.TargetQty)))
					continue
				}
			}

			// 3. 涨跌停检查：跌停不能卖出
			if currentPrice > 0 && downLimit > 0 {
				if err := market.CheckLimit(model.SideSell, currentPrice, upLimit, downLimit); err != nil {
					rejected = append(rejected, *reject(sig, "limit_down_sell", err.Error()))
					continue
				}
			}

			// 4. 卖出数量不能超过持仓数量
			if sig.TargetQty > pos.TotalQty {
				sig.TargetQty = pos.TotalQty
			}

			passed = append(passed, sig)
		}
		// DirHold 信号直接忽略
	}

	return passed, rejected
}

// CheckStopLoss 检查所有持仓的止损止盈
// 返回需要卖出的止损止盈信号
func (rm *RiskManager) CheckStopLoss(positions map[string]*model.Position,
	bars map[string]*model.Bar) []model.Signal {
	return rm.stopLossManager.CheckStopLoss(positions, bars)
}

// SectorExposure 获取各板块敞口
func (rm *RiskManager) SectorExposure(positions map[string]*model.Position,
	stocks map[string]*model.Stock, totalAsset float64) map[string]float64 {
	return rm.positionLimiter.SectorExposure(positions, stocks, totalAsset)
}
