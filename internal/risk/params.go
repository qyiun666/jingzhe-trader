// Package risk 档位参数覆盖的唯一定义处（GearTable）+ 仓位/资金核算 + 批次累计风控。
//
// 核心原则（ARCHITECTURE §8 / PRD P0-12-7）：
//   - Resolve 是纯函数：给定基准参数、档位、锁利开关与落后策略，输出生效风控参数；
//   - 物理熔断底线（单票 ≤60% / 总仓 ≤95%）永远生效，不受任何档位/策略放宽；
//   - Manager 批次内累计在途金额，同一批多笔买入合计超总仓位上限必须拒绝（历史 P0 bug）。
//
// 依赖方向：risk 只依赖 model，无 IO。
package risk

import (
	"jingzhe-trader/internal/model"
)

// StrategyBias 策略倾向：dynamic / trend_filter / exit_only。
type StrategyBias string

const (
	BiasDynamic     StrategyBias = "dynamic"
	BiasTrendFilter StrategyBias = "trend_filter"
	BiasExitOnly    StrategyBias = "exit_only"
)

// FactorWeights 五因子权重（与 screener.FactorWeights 数值语义一致；risk 侧独立定义避免反向依赖）。
type FactorWeights struct {
	Momentum  float64
	Quality   float64
	Value     float64
	LowVol    float64
	Liquidity float64
}

// RiskParams 生效风控参数（Resolve 的输出，唯一被 Manager/Sizing 消费的形态）。
type RiskParams struct {
	MaxTotalPositionPct float64       // 总仓位上限（占总资产）
	MaxPositionPct      float64       // 单票上限（占总资产）
	MaxSectorPct        float64       // 单一板块上限
	MaxPositions        int           // 最大持仓数（自适应）
	StopLossPct         float64       // 止损
	TrailingStopPct     float64       // 移动止盈回撤
	TakeProfitPct       float64       // 止盈
	AllowNewPosition    bool          // 是否允许开新仓
	MaxSingleAmountPct  float64       // 单笔金额上限（占总资产）
	MinSingleAmountFen  model.Fen     // 单笔金额下限（分）
	ScoreThresholdMul   float64       // 评分门槛倍数
	MinConfidence       float64       // 信号置信度下限
	Weights             FactorWeights // 因子权重
	Bias                StrategyBias  // 策略倾向
	TotalAsset          model.Fen     // 总资产（分；用于持仓数自适应与金额核算）
}

// gearSpec 单档位参数（GearTable 的行，ARCHITECTURE §8.2）。
type gearSpec struct {
	MaxTotalPositionPct float64
	MaxPositionPct      float64
	StopLossPct         float64
	TrailingStopPct     float64
	ScoreThresholdMul   float64
	MinConfidence       float64
	MaxSingleAmountPct  float64
	AllowNewPosition    bool
	Bias                StrategyBias
	Weights             FactorWeights
}

// 物理熔断底线（§8.3，永远生效、不可配置关闭）。
const (
	CircuitMaxSinglePct = 0.60 // 单票集中度硬上限
	CircuitMaxTotalPct  = 0.95 // 总仓位硬上限
)

// GearTable 档位参数表 —— 档位 → 参数覆盖关系的唯一定义处。
// G1 标准 / G2 收紧 / G3 防守（§8.2 表逐字段落地）。
var GearTable = map[model.Gear]gearSpec{
	model.GearG1: {
		MaxTotalPositionPct: 0.90,
		MaxPositionPct:      0.40,
		StopLossPct:         0.08,
		TrailingStopPct:     0.05,
		ScoreThresholdMul:   1.0,
		MinConfidence:       0.55,
		MaxSingleAmountPct:  1.0,
		AllowNewPosition:    true,
		Bias:                BiasDynamic,
		Weights:             FactorWeights{Momentum: 0.25, Quality: 0.20, Value: 0.20, LowVol: 0.15, Liquidity: 0.20},
	},
	model.GearG2: {
		MaxTotalPositionPct: 0.60,
		MaxPositionPct:      0.25,
		StopLossPct:         0.06,
		TrailingStopPct:     0.04,
		ScoreThresholdMul:   1.2,
		MinConfidence:       0.70,
		MaxSingleAmountPct:  0.5,
		AllowNewPosition:    true,
		Bias:                BiasTrendFilter,
		Weights:             FactorWeights{Momentum: 0.15, Quality: 0.30, Value: 0.20, LowVol: 0.25, Liquidity: 0.10},
	},
	model.GearG3: {
		MaxTotalPositionPct: 0.30,
		MaxPositionPct:      0.15,
		StopLossPct:         0.05,
		TrailingStopPct:     0.03,
		ScoreThresholdMul:   1.5, // G3 不适用评分门槛（不买新票），保留数值供查询
		MinConfidence:       1.0, // G3 禁新仓，置信度门槛失效
		MaxSingleAmountPct:  0.0, // 禁新仓
		AllowNewPosition:    false,
		Bias:                BiasExitOnly,
		Weights:             FactorWeights{Momentum: 0.20, Quality: 0.20, Value: 0.20, LowVol: 0.20, Liquidity: 0.20},
	},
}

// 默认单笔金额下限：5000 元（保费率 ≤0.1%，用户 1 万本金拍板值）。
const DefaultMinSingleAmountFen = model.Fen(5000 * 100)

// DefaultBase 返回基准参数（档位无关的部分 + 默认档位无关上限）。
// totalAsset 用于持仓数自适应与金额核算。
func DefaultBase(totalAsset model.Fen) RiskParams {
	return RiskParams{
		MaxSectorPct:       0.50,
		TakeProfitPct:      0.15,
		MinSingleAmountFen: DefaultMinSingleAmountFen,
		Weights:            FactorWeights{Momentum: 0.25, Quality: 0.20, Value: 0.20, LowVol: 0.15, Liquidity: 0.20},
		TotalAsset:         totalAsset,
	}
}

// ===================== 落后策略（Pace） =====================

// PaceAdjust 落后策略对风控参数的调整（§8.3）。实现必须保持纯函数语义。
// Batch 4 的 goal 包将提供带 GoalMetrics 的完整实现；本接口只暴露参数变换。
type PaceAdjust interface {
	Apply(p RiskParams) RiskParams
	Name() string
}

// NoPace 无调整（默认兜底）。
type NoPace struct{}

// Apply 原样返回。
func (NoPace) Apply(p RiskParams) RiskParams { return p }

// Name 策略名。
func (NoPace) Name() string { return "none" }

// UnrestrictedPace 用户拍板的完全放开模式（默认）：PaceGap 越大越激进，
// 放开仓位/止损修改，但受 GearTable 数值与物理熔断底线双重约束（§8.3 追赶曲线）。
type UnrestrictedPace struct {
	PaceGap        float64 // 时间进度 − 目标进度
	BudgetConsumed float64 // 回撤预算耗用（0~1）
}

// Apply 按 PaceGap 分档加码；BudgetConsumed ≥ 1.0 时强制防守（回撤预算耗尽）。
func (u UnrestrictedPace) Apply(p RiskParams) RiskParams {
	if u.BudgetConsumed >= 1.00 {
		// 熔断：强制 G3 防守 + 清空加成（全参数对齐 G3 档）
		g := GearTable[model.GearG3]
		p.MaxTotalPositionPct = g.MaxTotalPositionPct
		p.MaxPositionPct = g.MaxPositionPct
		p.MaxPositions = 2
		p.StopLossPct = g.StopLossPct
		p.TrailingStopPct = g.TrailingStopPct
		p.ScoreThresholdMul = g.ScoreThresholdMul
		p.MinConfidence = g.MinConfidence
		p.MaxSingleAmountPct = g.MaxSingleAmountPct
		p.AllowNewPosition = false
		p.Bias = g.Bias
		p.Weights = g.Weights
		return p
	}
	switch {
	case u.PaceGap < 0.15:
		// 档位原值
	case u.PaceGap < 0.30:
		p.MaxTotalPositionPct = minF(p.MaxTotalPositionPct*1.15, CircuitMaxTotalPct)
		p.StopLossPct = maxF(p.StopLossPct-0.01, 0.05)
	case u.PaceGap < 0.60:
		p.MaxTotalPositionPct = minF(p.MaxTotalPositionPct*1.30, CircuitMaxTotalPct)
		p.StopLossPct = maxF(p.StopLossPct-0.02, 0.04)
	default:
		p.MaxTotalPositionPct = minF(p.MaxTotalPositionPct*1.50, CircuitMaxTotalPct)
		p.StopLossPct = maxF(p.StopLossPct-0.03, 0.04)
	}
	return p
}

// Name 策略名。
func (UnrestrictedPace) Name() string { return "unrestricted" }

// ConservativePace 非默认的保守模式：只提高门槛，永不修改仓位/止损（§8.3 硬红线）。
type ConservativePace struct {
	PaceGap float64
}

// Apply 只调整 ScoreThresholdMul 与 MinConfidence。
func (c ConservativePace) Apply(p RiskParams) RiskParams {
	switch {
	case c.PaceGap < 0.15:
		// 无调整
	case c.PaceGap < 0.30:
		p.ScoreThresholdMul *= 1.1
		p.MinConfidence = maxF(p.MinConfidence, 0.60)
	default:
		p.ScoreThresholdMul *= 1.25
		p.MinConfidence = maxF(p.MinConfidence, 0.65)
	}
	return p
}

// Name 策略名。
func (ConservativePace) Name() string { return "conservative" }

// ===================== Resolve（纯函数） =====================

// Resolve 档位 → 生效风控参数的唯一覆盖链路：
// base（基准） → GearTable[gear]（档位覆盖） → 锁利叠加 → pace 落后策略 → 物理熔断收口。
// 无 IO，可全组合表驱动单测。
func Resolve(base RiskParams, gear model.Gear, profitLock bool, pace PaceAdjust) RiskParams {
	g, ok := GearTable[gear]
	if !ok {
		g = GearTable[model.GearG1] // 非法档位回落 G1（保守兜底）
	}
	p := base
	p.MaxTotalPositionPct = g.MaxTotalPositionPct
	p.MaxPositionPct = g.MaxPositionPct
	p.StopLossPct = g.StopLossPct
	p.TrailingStopPct = g.TrailingStopPct
	p.ScoreThresholdMul = g.ScoreThresholdMul
	p.MinConfidence = g.MinConfidence
	p.MaxSingleAmountPct = g.MaxSingleAmountPct
	p.AllowNewPosition = g.AllowNewPosition
	p.Bias = g.Bias
	p.Weights = g.Weights

	// 最大持仓数自适应（§8.2 #2b：<1万→2，<5万→2，<20万→4，否则 6）
	assetYuan := float64(p.TotalAsset) / 100
	adaptive := 2
	switch {
	case assetYuan >= 200000:
		adaptive = 6
	case assetYuan >= 50000:
		adaptive = 4
	}
	switch gear {
	case model.GearG2:
		p.MaxPositions = adaptive - 1 // 自适应 − 1（下限 2）
		if p.MaxPositions < 2 {
			p.MaxPositions = 2
		}
	case model.GearG3:
		p.MaxPositions = 2
	default: // G1 与其他回落档
		p.MaxPositions = adaptive
	}

	// 锁利叠加（§8.2）：MaxTotal = max(0.20, gear×0.70)；止损收紧到 ≤5%；禁开新仓
	if profitLock {
		p.MaxTotalPositionPct = maxF(p.MaxTotalPositionPct*0.70, 0.20)
		p.StopLossPct = minF(p.StopLossPct, 0.05)
		p.AllowNewPosition = false
	}

	// 落后策略调整
	if pace != nil {
		p = pace.Apply(p)
	}

	// 物理熔断收口（永远生效）
	if p.MaxPositionPct > CircuitMaxSinglePct {
		p.MaxPositionPct = CircuitMaxSinglePct
	}
	if p.MaxTotalPositionPct > CircuitMaxTotalPct {
		p.MaxTotalPositionPct = CircuitMaxTotalPct
	}
	return p
}

// minF / maxF 浮点辅助（避免引入 math只为两个数）。
func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
