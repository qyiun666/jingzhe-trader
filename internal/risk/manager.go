package risk

import (
	"fmt"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// RejectReason 拒绝原因
type RejectReason struct {
	TsCode string      // 股票代码
	Signal model.Signal // 原始信号
	Reason string      // 拒绝原因描述
	Rule   string      // 触发的规则名
}

// RiskManager 风控管理器
// 所有信号进入执行前都必须经过风控检查
// 检查顺序: 黑名单 -> 仓位限制 -> 止损止盈 -> 敞口控制 -> T+1
type RiskManager struct {
	cfg             config.RiskConfig
	positionLimiter *PositionLimiter
	stopLossManager *StopLossManager
	exposureManager *ExposureManager
	blacklist       *Blacklist
}

// NewRiskManager 创建风控管理器
func NewRiskManager(cfg config.RiskConfig) *RiskManager {
	return &RiskManager{
		cfg:             cfg,
		positionLimiter: NewPositionLimiter(cfg.MaxPositionPct, cfg.MaxTotalPositionPct, cfg.MaxSectorPct),
		stopLossManager: NewStopLossManager(cfg.StopLossPct, cfg.TakeProfitPct),
		exposureManager: NewExposureManager(cfg.MaxSectorPct),
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

// ExposureManager 获取敞口管理器
func (rm *RiskManager) ExposureManager() *ExposureManager {
	return rm.exposureManager
}

// Blacklist 获取黑名单
func (rm *RiskManager) Blacklist() *Blacklist {
	return rm.blacklist
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
					rejected = append(rejected, RejectReason{
						TsCode: sig.TsCode,
						Signal: sig,
						Reason: err.Error(),
						Rule:   "limit_up_buy",
					})
					continue
				}
			}

			// 2. 仓位限制检查（可能调整买入数量）
			adjusted, err := rm.positionLimiter.CheckPosition(sig, positions, totalAsset, stocks, currentPrice)
			if err != nil {
				// 如果调整后数量为 0，完全拒绝
				if adjusted.TargetQty <= 0 {
					rejected = append(rejected, RejectReason{
						TsCode: sig.TsCode,
						Signal: sig,
						Reason: err.Error(),
						Rule:   "position_limit",
					})
					continue
				}
				// 部分调整，继续后续检查
				sig = adjusted
			}

			// 3. 敞口控制检查（板块限制）
			if err := rm.exposureManager.CheckSectorLimit(sig, positions, stocks, totalAsset, currentPrice, sig.TargetQty); err != nil {
				rejected = append(rejected, RejectReason{
					TsCode: sig.TsCode,
					Signal: sig,
					Reason: err.Error(),
					Rule:   "sector_exposure",
				})
				continue
			}

			passed = append(passed, sig)

		} else if sig.Direction == model.DirSell {
			// 卖出信号检查

			pos := positions[sig.TsCode]

			// 1. 检查是否有持仓
			if pos == nil || pos.TotalQty <= 0 {
				rejected = append(rejected, RejectReason{
					TsCode: sig.TsCode,
					Signal: sig,
					Reason: "无持仓可卖",
					Rule:   "no_position",
				})
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
					rejected = append(rejected, RejectReason{
						TsCode: sig.TsCode,
						Signal: sig,
						Reason: fmt.Sprintf("T+1限制: 可卖量不足(可卖%d, 需卖%d)", pos.AvailableQty, sig.TargetQty),
						Rule:   "t1_restriction",
					})
					continue
				}
			}

			// 3. 涨跌停检查：跌停不能卖出
			if currentPrice > 0 && downLimit > 0 {
				if err := market.CheckLimit(model.SideSell, currentPrice, upLimit, downLimit); err != nil {
					rejected = append(rejected, RejectReason{
						TsCode: sig.TsCode,
						Signal: sig,
						Reason: err.Error(),
						Rule:   "limit_down_sell",
					})
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
	return rm.exposureManager.SectorExposure(positions, stocks, totalAsset)
}
