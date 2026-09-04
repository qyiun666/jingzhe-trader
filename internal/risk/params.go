// Package risk 档位参数覆盖的唯一定义处（GearTable）+ 仓位/资金核算 + 批次累计风控。
//
// 核心原则（ARCHITECTURE §8 / PRD P0-12-7）：
//   - Resolve 是纯函数：给定基准参数、档位、锁利开关与落后策略，输出生效风控参数；
//   - 物理熔断底线（单票 ≤60% / 总仓 ≤95%）永远生效，不受任何档位/策略放宽；
//   - Manager 批次内累计在途金额，同一批多笔买入合计超总仓位上限必须拒绝（历史 P0 bug）；
//   - 本包只做"硬截断"：要不要买、买多少由决策链（LLM）给出，这里负责把它砍回可承受范围。
//
// 依赖方向：risk 只依赖 model，无 IO。
package risk

import (
	"fmt"
	"jingzhe-trader/internal/model"
)

// StrategyBias 策略倾向：dynamic / trend_filter / exit_only。
type StrategyBias string

const (
	BiasDynamic     StrategyBias = "dynamic"
	BiasTrendFilter StrategyBias = "trend_filter"
	BiasExitOnly    StrategyBias = "exit_only"
)

// RiskParams 生效风控参数（Resolve 的输出，唯一被 Manager/Sizing 消费的形态）。
//
// 这里只有"硬截断"：单票/总仓/持仓数/一手价/金额下限/置信度下限。
// 综合分不做门槛（它是选股漏斗与 prompt 的证据，不是否决权），故本结构不含因子权重。
type RiskParams struct {
	MaxTotalPositionPct float64      // 总仓位上限（占总资产）
	MaxPositionPct      float64      // 单票上限（占总资产）
	MaxSectorPct        float64      // 单一板块上限
	MaxPositions        int          // 最大持仓数（自适应）
	StopLossPct         float64      // 止损
	TrailingStopPct     float64      // 移动止盈回撤
	TakeProfitPct       float64      // 止盈
	AllowNewPosition    bool         // 是否允许开新仓
	MaxSingleAmountPct  float64      // 单笔金额上限（占总资产）
	MinSingleAmountFen  model.Fen    // 单笔金额下限（分，见 MinAmountFloor 的小资金收口）
	MinConfidence       float64      // 决策置信度下限（模型自报值）
	Bias                StrategyBias // 策略倾向
	TotalAsset          model.Fen    // 总资产（分；用于持仓数自适应与金额核算）
}

// SingleCapFen 单票实际可下单的上限（分）：单票上限与单笔金额上限取严。
func (p RiskParams) SingleCapFen() model.Fen {
	single := pctOf(p.TotalAsset, p.MaxPositionPct)
	if cap2 := pctOf(p.TotalAsset, p.MaxSingleAmountPct); cap2 < single {
		return cap2
	}
	return single
}

// MinAmountFloor 生效的单笔金额下限（分）。
//
// 5000 元这个绝对值的唯一用途是让最低 5 元佣金不吃掉 0.1% 本金；但两万元级别的账户
// 单票上限本身就不到一万元，按绝对下限会把每一个候选都判成"金额过小"而全数否决。
// 因此取"绝对下限"与"单票上限的一半"的较小值：大账户仍受 5000 元约束，小账户自动缩放。
func (p RiskParams) MinAmountFloor() model.Fen {
	if half := p.SingleCapFen() / 2; half > 0 && half < p.MinSingleAmountFen {
		return half
	}
	return p.MinSingleAmountFen
}

// gearSpec 单档位参数（GearTable 的行，ARCHITECTURE §8.2）。
type gearSpec struct {
	MaxTotalPositionPct float64
	MaxPositionPct      float64
	StopLossPct         float64
	TrailingStopPct     float64
	MinConfidence       float64
	MaxSingleAmountPct  float64
	AllowNewPosition    bool
	Bias                StrategyBias
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
		MinConfidence:       0.55,
		MaxSingleAmountPct:  1.0,
		AllowNewPosition:    true,
		Bias:                BiasDynamic,
	},
	model.GearG2: {
		MaxTotalPositionPct: 0.60,
		MaxPositionPct:      0.25,
		StopLossPct:         0.06,
		TrailingStopPct:     0.04,
		MinConfidence:       0.70,
		MaxSingleAmountPct:  0.5,
		AllowNewPosition:    true,
		Bias:                BiasTrendFilter,
	},
	model.GearG3: {
		MaxTotalPositionPct: 0.30,
		MaxPositionPct:      0.15,
		StopLossPct:         0.05,
		TrailingStopPct:     0.03,
		MinConfidence:       1.0, // G3 禁新仓，置信度门槛失效
		MaxSingleAmountPct:  0.0, // 禁新仓
		AllowNewPosition:    false,
		Bias:                BiasExitOnly,
	},
}

// 默认单笔金额下限：5000 元（保费率 ≤0.1%；小资金账户由 MinAmountFloor 自动缩放）。
const DefaultMinSingleAmountFen = model.Fen(5000 * 100)

// DefaultBase 返回基准参数（档位无关的部分 + 默认档位无关上限）。
// totalAsset 用于持仓数自适应与金额核算。
func DefaultBase(totalAsset model.Fen) RiskParams {
	return RiskParams{
		MaxSectorPct:       0.50,
		TakeProfitPct:      0.15,
		MinSingleAmountFen: DefaultMinSingleAmountFen,
		TotalAsset:         totalAsset,
	}
}

// ===================== 落后策略（Pace） =====================

// PaceAdjust 落后策略对风控参数的调整（§8.3）。实现必须保持纯函数语义。
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
		p.MinConfidence = g.MinConfidence
		p.MaxSingleAmountPct = g.MaxSingleAmountPct
		p.AllowNewPosition = false
		p.Bias = g.Bias
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

// ConservativePace 非默认的保守模式：只抬高置信度门槛，永不修改仓位/止损（§8.3 硬红线）。
type ConservativePace struct {
	PaceGap float64
}

// Apply 只抬高 MinConfidence。
func (c ConservativePace) Apply(p RiskParams) RiskParams {
	switch {
	case c.PaceGap < 0.15:
		// 无调整
	case c.PaceGap < 0.30:
		p.MinConfidence = maxF(p.MinConfidence, 0.60)
	default:
		p.MinConfidence = maxF(p.MinConfidence, 0.65)
	}
	return p
}

// Name 策略名。
func (ConservativePace) Name() string { return "conservative" }

// ===================== Resolve（纯函数） =====================

// Resolve 档位 → 生效风控参数的唯一覆盖链路：
// base（基准） → GearTable[gear]（档位覆盖） → 锁利叠加 → pace 落后策略 → 物理熔断收口。
//
// 档位必须在表里，否则报错：map 未命中得到的是零值 gearSpec，而零值里
// StopLossPct=0 意味着"成本价即止损线"，每个持仓都会被判定止损 —— 未知档位
// 不是"最保守"，是"最激进地把仓位清光"，所以这里必须拒绝而不是继续算。
func Resolve(base RiskParams, gear model.Gear, profitLock bool, pace PaceAdjust) (RiskParams, error) {
	g, ok := GearTable[gear]
	if !ok {
		return RiskParams{}, fmt.Errorf("未知风控档位 %q：GearTable 里没有这一行（可选 G1|G2|G3）", string(gear))
	}
	p := base
	p.MaxTotalPositionPct = g.MaxTotalPositionPct
	p.MaxPositionPct = g.MaxPositionPct
	p.StopLossPct = g.StopLossPct
	p.TrailingStopPct = g.TrailingStopPct
	p.MinConfidence = g.MinConfidence
	p.MaxSingleAmountPct = g.MaxSingleAmountPct
	p.AllowNewPosition = g.AllowNewPosition
	p.Bias = g.Bias

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
	default: // G1：表里只剩这三档，未知档位已在函数开头报错
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
	return p, nil
}

// minF / maxF 浮点辅助（避免引入 math 只为两个数）。
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
