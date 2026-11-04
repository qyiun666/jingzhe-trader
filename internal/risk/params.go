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
	MaxTotalPositionPct float64 // 总仓位上限（占总资产）
	MaxPositionPct      float64 // 单票上限（占总资产）
	MaxPositions        int     // 最大持仓数（自适应）
	StopLossPct         float64 // 止损
	TrailingStopPct     float64 // 移动止盈回撤
	TrailingArmPct      float64 // 移动止盈激活线：高点≥成本×(1+arm) 才启用；0=旧口径
	// TrailingArmSafetyPct 动态武装线的安全垫：在"刚好盖住双边摩擦"的武装线上再加的
	// 比例（见 TrailingArmPctFor）。0 = 只盖摩擦、不留利润。
	TrailingArmSafetyPct float64
	TakeProfitPct        float64   // 止盈
	AllowNewPosition     bool      // 是否允许开新仓
	MaxSingleAmountPct   float64   // 单笔金额上限（占总资产）
	MinSingleAmountFen   model.Fen // 单笔金额下限（分，见 MinAmountFloor 的小资金收口）
	MinAmountRatio       float64   // 单笔下限占单票上限的比例（默认 0.5；弱势试探提高到 0.75）
	MinConfidence        float64   // 决策置信度下限（模型自报值）
	// ExpectedExcessPct 摩擦闸的输入：预估持有期超额（小数，0.005=0.5%）。
	// 取值只能来自归因的"买入 vs 跳过"差值，且**不显著时必须填 0**：0 意味着
	// 任何正摩擦都会被拦（组长 20261103 定参①，是设计意图而不是缺陷）。
	ExpectedExcessPct float64
	// WeakBuyAllowed 弱势试探期是否允许下发买入指令单（config signal.weak_buy_enabled）。
	// 默认 false（新政策）：true 只是回退开关，由 WeakParams 消费成 BuysBlocked。
	WeakBuyAllowed bool
	// WeakProbe 本轮处于"门槛关闭且判定弱势"的小仓位试探档（WeakParams 实际生效的标记）。
	// 它是买入闸与规则 4 停用**共用的同一条判据**：两条政策都以 run_trace 里那条
	// alert:WEAK_REGIME（"门槛关闭且大盘弱势，进入小仓位试探模式"）为准，
	// 不允许一条按配置开关、另一条按指数判据，那会在边界日出现
	// "买单已停但还在按排名淘汰换仓"这种自相矛盾的形态。
	WeakProbe bool
	// BuysBlocked 本轮一律不下发买入指令单：弱势试探期买入闸的结果位，
	// 由 WeakParams 在 WeakBuyAllowed=false 时置位，Manager 据此以 weak_buy_disabled 拒绝。
	// 买入评审链、影子清单与归因留痕不受它影响（拦的是"下单"，不是"判断"）。
	BuysBlocked bool
	// MaxPerIndustry 组合同行业持仓只数上限（0=关闭）。默认档 2，弱势档 1；
	// 只约束"开新仓"，已持有标的加仓不占新名额。行业未知时降级放行。
	MaxPerIndustry int
	TotalAsset     model.Fen // 总资产（分；用于持仓数自适应与金额核算）
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
	return minAmountFen(p.SingleCapFen(), ratio, p.MinSingleAmountFen)
}

// MinAmountFor 只凭"单票上限 + 是否弱势档"推导生效单笔下限（分）。
//
// 存在的理由是漏斗与风控必须用同一条下限线：MinAmountFloor 是 RiskParams 的方法，
// 而选股漏斗手里只有单票上限（Budget.CapFen）与弱势旗标，拿不到完整 rp——把算式抽成
// 这个纯函数、由 MinAmountFloor 同样调用，比让 screener 再抄一遍"0.75 × 上限"安全
// （抄一遍就会在两处分别漂移，而漂移的症状是"候选进来了却被风控全斩"）。
// 绝对下限取 DefaultMinSingleAmountFen：它不是配置键（见 keys.go 对 cost 段的说明），
// 所以这里不需要参数化。
func MinAmountFor(singleCapFen model.Fen, weak bool) model.Fen {
	ratio := baseMinAmountRatio
	if weak {
		ratio = WeakMinAmountRatio
	}
	return minAmountFen(singleCapFen, ratio, DefaultMinSingleAmountFen)
}

// minAmountFen 单笔下限的唯一算式：绝对下限与"单票上限 × 比例"取严。
func minAmountFen(singleCapFen model.Fen, ratio float64, absFloor model.Fen) model.Fen {
	if byRatio := pctOf(singleCapFen, ratio); byRatio > 0 && byRatio < absFloor {
		return byRatio
	}
	return absFloor
}

// FrictionPct 给定成交金额的双边摩擦率（小数）：双边成本 / 名义额。
//
// 最低 5 元佣金让这项与名义额成反比——12,000 元账户的单票上限级别（约 1,200 元）
// 是 0.9% 左右，600 元级是 1.8% 左右。它是"这笔交易值不值得做"的唯一硬成本，
// 所以摩擦闸与动态武装线都用它，而不是用名义费率。金额非正返回 1（最保守：
// 无法核算成本时按"成本高到吃光收益"处理，而不是当成 0 放行）。
func FrictionPct(amount model.Fen) float64 {
	return frictionPct(amount, false)
}

// FrictionPctFund 场内基金档的双边摩擦率（无卖出印花税）。
func FrictionPctFund(amount model.Fen) float64 {
	return frictionPct(amount, true)
}

// FrictionPctFor 按标的品种取双边摩擦率：isFund=true 走场内基金档。
func FrictionPctFor(amount model.Fen, isFund bool) float64 {
	return frictionPct(amount, isFund)
}

func frictionPct(amount model.Fen, fund bool) float64 {
	if amount <= 0 {
		return 1
	}
	return float64(RoundTripCostFenFor(amount, fund)) / float64(amount)
}

// TrailingArmPctFor 按当笔名义额推导移动止盈武装线（20261103 定参②）。
//
// 为什么不能是全局常数：触发价 = 成本×(1+arm)×(1−d)，要净赚就必须
// (1+arm)(1−d) − 1 > f，而 f 随名义额变化（最低佣金）。所以
//
//	arm_min = (1 + f) / (1 − d) − 1        （打平，一分利润都不留）
//	arm_eff = arm_min + 安全垫              （默认再留 1pp）
//
// 最终取 max(静态下限, arm_eff)：静态下限保证"就算算出来的很小也不许低于 8%"，
// 动态项保证"单子越小、武装线越高"——600 元的单子要涨约 8.2% 才允许武装。
//
// TrailingArmPct <= 0 是显式的旧口径开关（回到"成本线即武装"），此时不做动态推导，
// 否则"设 0 回滚"会被动态项悄悄覆盖，回退开关就形同虚设。
func (p RiskParams) TrailingArmPctFor(notional model.Fen) float64 {
	return p.trailingArm(notional, false)
}

// TrailingArmPctForFund 场内基金档的武装线：算式同 TrailingArmPctFor，摩擦项免印花税。
func (p RiskParams) TrailingArmPctForFund(notional model.Fen) float64 {
	return p.trailingArm(notional, true)
}

// TrailingArmPctForVariant 按标的品种取武装线（isFund=true 走场内基金档）。
func (p RiskParams) TrailingArmPctForVariant(notional model.Fen, isFund bool) float64 {
	return p.trailingArm(notional, isFund)
}

func (p RiskParams) trailingArm(notional model.Fen, fund bool) float64 {
	if p.TrailingArmPct <= 0 {
		return 0
	}
	d := p.TrailingStopPct
	if d <= 0 || d >= 1 {
		d = baseTrailingStopPct // 坏值回落基准，不让回撤线把分母变成 0 或负数
	}
	safety := p.TrailingArmSafetyPct
	if safety < 0 {
		safety = baseTrailingArmSafetyPct
	}
	arm := (1+frictionPct(notional, fund))/(1-d) - 1 + safety
	if arm < p.TrailingArmPct {
		return p.TrailingArmPct
	}
	return arm
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
	// baseTrailingArmPct 移动止盈武装线的**静态下限**（高点至少 +8% 才启用）。
	// 从 0.06 提到 0.08 的理由是算术而非手感：arm=6% 配 d=5% 时触发价下限
	// = 1.06×0.95 = 成本×1.007，只锁 +0.7% 毛利，而 600~1,000 元小单的双边摩擦
	// 实测 1.09%~1.78% —— 止盈只要在武装线附近触发就注定净亏（20261103 601328）。
	// 实际生效值由 TrailingArmPctFor 按当笔名义额动态推导，这里只是不可低于的下限。
	baseTrailingArmPct       = 0.08
	baseTrailingArmSafetyPct = 0.01 // 动态武装线的安全垫（盖住摩擦之外再留 1pp 利润）
	baseTakeProfitPct        = 0.15 // 止盈
	baseMaxSingleAmountPct   = 1.0  // 单笔金额上限（占总资产）
	baseMinConfidence        = 0.55 // 决策置信度下限
	baseMinAmountRatio       = 0.5  // 单笔下限 = 单票上限 × 该比例（与绝对下限取小）
	baseMaxPerIndustry       = 2    // 组合同行业持仓上限（默认档）
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
	// 弱势试探期买入闸（20261103 定参①）：判到弱势就把"下单"这一侧整体关掉。
	// 只关下单，不关评审——漏斗、影子清单与归因留痕继续跑，样本零成本积累；
	// 已有的持仓仍由价格型三线与弱势减仓管理（下面两条不动是刻意的：
	// 弱势下的"快跑"能力比"少亏"能力更值钱）。
	if !p.WeakBuyAllowed {
		p.BuysBlocked = true
	}
	// 弱势试探档标记（规则 4 停用与买入闸共用的判据，见 RiskParams.WeakProbe）。
	p.WeakProbe = true
	// 弱势档同行业最多 1 只：行业相关性在下跌市里被放大，2 只同行业等于一只
	// 双倍仓位。0=关闭时保持关闭（不替用户推翻显式配置）。
	if p.MaxPerIndustry > 0 && p.MaxPerIndustry > 1 {
		p.MaxPerIndustry = 1
	}
	return p
}

// BuyIndustryCap 由候选侧每行业上限推导买入侧组合上限：min(默认档 2, 配置值)。
//
// 两层的默认值不同是刻意的：候选池需要行业分散到 3 只才不至于把强势行业一刀切，
// 而账户只有 2 个持仓名额，同行业持有 2 只已经是集中度上限。配置把候选侧收紧到
// 0/1 时，买入侧跟随收紧（0=关闭）；配得比 2 宽也只放宽到 2。
func BuyIndustryCap(candidateCap int) int {
	if candidateCap <= 0 {
		return 0
	}
	if candidateCap > baseMaxPerIndustry {
		return baseMaxPerIndustry
	}
	return candidateCap
}

// DefaultParams 给定总资产，输出生效风控参数：固定基准 → 持仓数自适应 → 物理熔断收口。
//
// totalAsset 用于持仓数自适应与金额核算。持仓数自适应（<5万→2，<20万→4，否则 6）
// 保证小资金账户不会把仓位摊到十几只票上、每只都不够一手。
func DefaultParams(totalAsset model.Fen) RiskParams {
	p := RiskParams{
		MaxTotalPositionPct:  baseMaxTotalPositionPct,
		MaxPositionPct:       baseMaxPositionPct,
		StopLossPct:          baseStopLossPct,
		TrailingStopPct:      baseTrailingStopPct,
		TrailingArmPct:       baseTrailingArmPct,
		TrailingArmSafetyPct: baseTrailingArmSafetyPct,
		TakeProfitPct:        baseTakeProfitPct,
		AllowNewPosition:     true,
		MaxSingleAmountPct:   baseMaxSingleAmountPct,
		MinConfidence:        baseMinConfidence,
		MinSingleAmountFen:   DefaultMinSingleAmountFen,
		MinAmountRatio:       baseMinAmountRatio,
		MaxPerIndustry:       baseMaxPerIndustry,
		// 两项都是"键缺失时=新政策"的取严默认（组长 20261103 定参①）：
		// 弱势试探期不允许买、没有显著归因证据时预期超额按 0（=摩擦全拦）。
		// 老库里没有这两个键时，行为与今天的定参一致，不需要先写键才生效。
		WeakBuyAllowed:    false,
		ExpectedExcessPct: 0,
		TotalAsset:        totalAsset,
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
