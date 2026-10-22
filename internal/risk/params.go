// Package risk 生效风控参数 + 仓位/资金核算 + 批次累计风控。
//
// 核心原则：
//   - DefaultParams 是纯函数：给定总资产，输出生效风控参数（固定基准 + 持仓数自适应 + 物理熔断收口）；
//   - 物理熔断底线（单票 ≤60% / 总仓 ≤95%）永远生效，不可放宽；
//   - Manager 批次内累计在途金额，同一批多笔买入合计超总仓位上限必须拒绝（历史 P0 bug）；
//   - 本包只做"硬截断"：要不要买、买多少由决策链（LLM）给出，这里负责把它砍回可承受范围。
//
// 依赖方向：risk 只依赖 model，无 IO。
package risk

import "jingzhe-trader/internal/model"

// RiskParams 生效风控参数（唯一被 Manager/Sizing 消费的形态）。
//
// 这里只有"硬截断"：单票/总仓/持仓数/一手价/金额下限/置信度下限。
// 综合分不做门槛（它是选股漏斗与 prompt 的证据，不是否决权），故本结构不含因子权重。
type RiskParams struct {
	MaxTotalPositionPct float64   // 总仓位上限（占总资产）
	MaxPositionPct      float64   // 单票上限（占总资产）
	MaxPositions        int       // 最大持仓数（自适应）
	StopLossPct         float64   // 止损
	TrailingStopPct     float64   // 移动止盈回撤
	TrailingArmPct      float64   // 移动止盈激活线：高点≥成本×(1+arm) 才启用；0=旧口径
	TakeProfitPct       float64   // 止盈
	AllowNewPosition    bool      // 是否允许开新仓
	MaxSingleAmountPct  float64   // 单笔金额上限（占总资产）
	MinSingleAmountFen  model.Fen // 单笔金额下限（分，见 MinAmountFloor 的小资金收口）
	MinAmountRatio      float64   // 单笔下限占单票上限的比例（默认 0.5；弱势试探提高到 0.75）
	MinConfidence       float64   // 决策置信度下限（模型自报值）
	TotalAsset          model.Fen // 总资产（分；用于持仓数自适应与金额核算）
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
// 因此取"绝对下限"与"单票上限 × MinAmountRatio"的较小值：大账户仍受 5000 元约束，
// 小账户自动缩放；弱势试探档把比例从 0.5 提到 0.75，避免 600 元级的单子被佣金吃光。
func (p RiskParams) MinAmountFloor() model.Fen {
	ratio := p.MinAmountRatio
	if ratio <= 0 || ratio > 1 {
		ratio = baseMinAmountRatio // 坏值/旧构造回落到基准，不静默放行
	}
	if byRatio := pctOf(p.SingleCapFen(), ratio); byRatio > 0 && byRatio < p.MinSingleAmountFen {
		return byRatio
	}
	return p.MinSingleAmountFen
}

// 物理熔断底线（永远生效、不可配置关闭）。
const (
	CircuitMaxSinglePct = 0.60 // 单票集中度硬上限
	CircuitMaxTotalPct  = 0.95 // 总仓位硬上限
)

// 生效风控基准（固定常数值，删除档位状态机后不再有档位覆盖）。
// 取原 G1 标准档：原默认档位即 G1，固定成一套使行为与"未落后未防守"时一致。
const (
	baseMaxTotalPositionPct = 0.90 // 总仓位上限
	baseMaxPositionPct      = 0.40 // 单票上限
	baseStopLossPct         = 0.08 // 止损
	baseTrailingStopPct     = 0.05 // 移动止盈回撤
	baseTrailingArmPct      = 0.06 // 移动止盈激活线（高点至少 +6% 才启用）
	baseTakeProfitPct       = 0.15 // 止盈
	baseMaxSingleAmountPct  = 1.0  // 单笔金额上限（占总资产）
	baseMinConfidence       = 0.55 // 决策置信度下限
	baseMinAmountRatio      = 0.5  // 单笔下限 = 单票上限 × 该比例（与绝对下限取小）
)

// 默认单笔金额下限：5000 元（保费率 ≤0.1%；小资金账户由 MinAmountFloor 自动缩放）。
const DefaultMinSingleAmountFen = model.Fen(5000 * 100)

// 弱势试探基准（组长 20260924 批准）：门槛关闭（screen.gate_enabled=false）且大盘
// 判定为弱势时，仓位上限收缩到这组值。12000 元账户 ≈ 每笔最多 1200 元，
// 单笔最大亏损（8% 止损）≈ 96 元，全账户弱势日敞口 ≤1.6%。
const (
	WeakMaxTotalPctDefault  = 0.20
	WeakMaxSinglePctDefault = 0.10
	// WeakMinAmountRatio 弱势档单笔下限占单票上限的比例（默认档 0.5）。
	// 弱势单票约 1200 元，600 元的单子双边佣金+印花税 ≈ 10.7 元（1.8%），
	// 提到 0.75 后单笔至少约 900 元，成本占比降到约 1.2%。
	WeakMinAmountRatio = 0.75
)

// WeakParams 把风控参数收缩到弱势试探口径：只收紧、永不放宽——
// 上限配置坏值不静默放行：越界值按物理熔断钳回，非正/不可解析回落到默认档。
// 止损/止盈/持仓数/置信度不动：弱势下的"快跑"能力比"少亏"能力更值钱。
func WeakParams(p RiskParams, maxTotalPct, maxSinglePct float64) RiskParams {
	if maxTotalPct <= 0 || maxTotalPct > CircuitMaxTotalPct {
		if maxTotalPct <= 0 {
			maxTotalPct = WeakMaxTotalPctDefault
		} else {
			maxTotalPct = CircuitMaxTotalPct
		}
	}
	if maxSinglePct <= 0 || maxSinglePct > CircuitMaxSinglePct {
		if maxSinglePct <= 0 {
			maxSinglePct = WeakMaxSinglePctDefault
		} else {
			maxSinglePct = CircuitMaxSinglePct
		}
	}
	if maxSinglePct > maxTotalPct {
		maxSinglePct = maxTotalPct
	}
	if p.MaxTotalPositionPct > maxTotalPct {
		p.MaxTotalPositionPct = maxTotalPct
	}
	if p.MaxPositionPct > maxSinglePct {
		p.MaxPositionPct = maxSinglePct
	}
	// 仓位上限收缩后，单笔下限按新单票上限重新取比例：不提高比例的话，
	// 试探档会允许 600 元级交易，双边成本占比接近 2%。
	p.MinAmountRatio = WeakMinAmountRatio
	return p
}

// DefaultParams 给定总资产，输出生效风控参数：固定基准 → 持仓数自适应 → 物理熔断收口。
//
// totalAsset 用于持仓数自适应与金额核算。持仓数自适应（<5万→2，<20万→4，否则 6）
// 保证小资金账户不会把仓位摊到十几只票上、每只都不够一手。
func DefaultParams(totalAsset model.Fen) RiskParams {
	p := RiskParams{
		MaxTotalPositionPct: baseMaxTotalPositionPct,
		MaxPositionPct:      baseMaxPositionPct,
		StopLossPct:         baseStopLossPct,
		TrailingStopPct:     baseTrailingStopPct,
		TrailingArmPct:      baseTrailingArmPct,
		TakeProfitPct:       baseTakeProfitPct,
		AllowNewPosition:    true,
		MaxSingleAmountPct:  baseMaxSingleAmountPct,
		MinConfidence:       baseMinConfidence,
		MinSingleAmountFen:  DefaultMinSingleAmountFen,
		MinAmountRatio:      baseMinAmountRatio,
		TotalAsset:          totalAsset,
	}

	assetYuan := float64(totalAsset) / 100
	switch {
	case assetYuan >= 200000:
		p.MaxPositions = 6
	case assetYuan >= 50000:
		p.MaxPositions = 4
	default:
		p.MaxPositions = 2
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
