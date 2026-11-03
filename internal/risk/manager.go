package risk

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/model"
)

// 否决规则名（落 order_ticket 记录与日志，验收 #7/#8 的取值契约）。
const (
	RuleAllowNewPosition = "allow_new_position"
	// RuleBuysBlocked 弱势试探期买入闸（20261103 定参①）：整条买入链关掉。
	// 与 allow_new_position 分开命名，是为了让事后归因能分清"弱势不许买"和
	// "风控本来就不允许开新仓"——前者是政策，后者是账户结构。
	RuleBuysBlocked = "weak_buy_disabled"
	// RuleFrictionGate 摩擦闸：预估双边摩擦 ≥ 预估持有期超额，这笔交易不值得做。
	RuleFrictionGate       = "friction_gate"
	RuleMinConfidence      = "min_confidence"
	RuleMaxPositions       = "max_positions"
	RuleAlreadyHolding     = "already_holding" // 保留为契约取值；已持有现在走加仓路径，不再据此拒绝
	RuleSinglePositionFull = "single_position_full"
	RuleIndustryCap        = "max_per_industry"
	RuleLotUnaffordable    = "lot_unaffordable"
	RuleMinAmount          = "min_amount"
	RuleCashInsufficient   = "cash_insufficient"
	RuleMaxTotalPosition   = "max_total_position"
	RuleIllegalPrice       = "illegal_price"
)

// AccountState 风控核算的账户状态快照（金额一律分）。
type AccountState struct {
	TotalAsset    model.Fen            // 总资产
	Cash          model.Fen            // 可用现金
	PositionsMV   model.Fen            // 持仓市值（成本或最新价口径，由调用方决定）
	PositionCount int                  // 当前持仓只数
	HeldCodes     map[string]bool      // 已持有代码集合
	HeldMV        map[string]model.Fen // 已持有代码 → 当前市值（成本口径），加仓时算剩余单票额度
	// HeldIndustry 已持有代码 → 行业（stock_basic 查询结果；缺失为空）。
	// 行业上限只数按非空值统计；未知行业不阻断，由调用方记 Notes 后放行。
	HeldIndustry map[string]string
}

// BuyIntent 单笔买入意向：决策链给出"买这只、想要这么多钱"，Manager 负责砍到硬上限之内。
type BuyIntent struct {
	TsCode     string
	Name       string
	Industry   string // 行业（空=未知，行业上限降级放行）
	IsETF      bool   // ETF 不参与个股行业集中度上限
	RefPrice   model.Fen
	Confidence float64   // 决策自报置信度 0~1
	WantFen    model.Fen // 期望投入金额（分）；超过单票上限会被截断
}

// Decision 批次风控裁决结果。
type Decision struct {
	TsCode     string
	Approved   bool
	RejectRule string // 否决时非空（100% 留痕，禁静默丢弃）
	RejectMsg  string
	// Note 通过时附带的降级说明（如行业未知、行业上限降级放行），供上层写 Report.Notes。
	Note       string
	Qty        model.Qty // 通过时给出的建议股数（整手）
	Amount     model.Fen // 通过时的计划金额（分）
	PlannedPct float64   // 计划金额占总资产比例
}

// Manager 批次累计风控管理器：同一批多笔买入按序核算，
// 已通过的在途金额即时累计，合计超总仓位上限必须拒绝后续笔（历史 P0 bug 回归点）。
type Manager struct {
	P RiskParams
}

// NewManager 构造管理器。
func NewManager(p RiskParams) *Manager { return &Manager{P: p} }

// CheckBatch 按序裁决一批买入意向。
//
// 累计语义：committed = 现有持仓市值 + 本批已通过金额；每笔通过后立即累加，
// 因此第 N 笔即使单笔未超单票上限，只要使合计突破总仓位上限就会被拒，
// reject_rule 固定为 max_total_position（验收 #7）。
func (m *Manager) CheckBatch(intents []BuyIntent, st AccountState) []Decision {
	out := make([]Decision, 0, len(intents))
	committed := st.PositionsMV
	posCount := st.PositionCount
	totalCap := pctOf(st.TotalAsset, m.P.MaxTotalPositionPct)
	singleCap := m.P.SingleCapFen()
	minAmount := m.P.MinAmountFloor()
	lotCost := func(price model.Fen) model.Fen { return price.Mul(model.LotShares) }
	// 本批会就地累计"已持有/敞口"（同一批里先通过的新仓，后续同码即视为加仓），
	// 故两个 map 都必须可写：调用方给 nil 时在此补齐，避免写 nil map panic。
	if st.HeldCodes == nil {
		st.HeldCodes = make(map[string]bool, len(intents))
	}
	if st.HeldMV == nil {
		st.HeldMV = make(map[string]model.Fen, len(intents))
	}
	// 组合行业只数：只统计已知行业；未知行业不阻断（降级放行）。
	industryCount := make(map[string]int)
	if m.P.MaxPerIndustry > 0 {
		for code := range st.HeldCodes {
			if ind := strings.TrimSpace(st.HeldIndustry[code]); ind != "" {
				industryCount[ind]++
			}
		}
	}

	for _, in := range intents {
		dec := Decision{TsCode: in.TsCode}
		reject := func(rule, msg string) {
			dec.Approved = false
			dec.RejectRule = rule
			dec.RejectMsg = msg
		}

		// 0) 弱势试探期买入闸（20261103 定参①）：判到弱势就整条买入链关掉。
		// 放在最前面是因为它优先于一切——加仓也拦（试探仓的轮换本身就是被拦的行为），
		// 且拦单不拦判决：LLM 评审、影子清单、screen:topn 与归因留痕都在上游照常完成，
		// 样本零成本继续积累，回退只需把 signal.weak_buy_enabled 设回 true。
		if m.P.BuysBlocked {
			reject(RuleBuysBlocked, "弱势试探期买入闸：门槛关闭且大盘弱势，本轮不下发任何买入指令单"+
				"（评审与归因留痕照跑；回退键 signal.weak_buy_enabled=true）")
			out = append(out, dec)
			continue
		}
		// 1) 开新仓开关
		if !m.P.AllowNewPosition {
			reject(RuleAllowNewPosition, "当前风控参数禁止开新仓")
			out = append(out, dec)
			continue
		}
		// 2) 决策置信度下限（决策方自报值，风控保留的唯一质量门槛）
		if in.Confidence < m.P.MinConfidence {
			reject(RuleMinConfidence, fmt.Sprintf("决策置信度 %.2f 低于下限 %.2f", in.Confidence, m.P.MinConfidence))
			out = append(out, dec)
			continue
		}
		// 3) 持仓数与重复持仓。
		//
		// 已持有的标的不是"重复建仓"而是**加仓**：它不占新的持仓名额，
		// 但仍受单票上限约束——额度是"单票上限 − 该票现有敞口"，加满是上限不是无限加。
		isAdd := st.HeldCodes[in.TsCode]
		if !isAdd && posCount >= m.P.MaxPositions {
			reject(RuleMaxPositions, fmt.Sprintf("持仓数 %d（含本批在途）达到上限 %d", posCount, m.P.MaxPositions))
			out = append(out, dec)
			continue
		}
		// 4) 行业集中度：只约束开新仓；同行业已持有 N 只时不再开第 N+1 只。
		// ETF 不占个股行业名额；行业未知时无法判定，降级放行并留 Notes（禁静默阻断）。
		ind := strings.TrimSpace(in.Industry)
		if !isAdd && !in.IsETF && m.P.MaxPerIndustry > 0 {
			if ind == "" {
				dec.Note = fmt.Sprintf("%s 行业未知，行业上限降级放行", in.TsCode)
			} else if industryCount[ind] >= m.P.MaxPerIndustry {
				reject(RuleIndustryCap, fmt.Sprintf("同行业已持有 %d 只，达到行业上限 %d 只（%s）",
					industryCount[ind], m.P.MaxPerIndustry, ind))
				out = append(out, dec)
				continue
			}
		}
		// 5) 价格合法性与金额核算：以决策要求的金额为准，被单票上限与现金双重截断
		if in.RefPrice <= 0 {
			reject(RuleIllegalPrice, fmt.Sprintf("参考价非法: %d 分", int64(in.RefPrice)))
			out = append(out, dec)
			continue
		}
		// 单票可用额度：新仓 = 单票上限；加仓 = 单票上限 − 已占用敞口。
		roomCap := singleCap
		if isAdd {
			roomCap = singleCap - st.HeldMV[in.TsCode]
			if roomCap <= 0 {
				reject(RuleSinglePositionFull, fmt.Sprintf("该标的已占用 %s 元，已达单票上限 %s 元，无可加额度",
					st.HeldMV[in.TsCode], singleCap))
				out = append(out, dec)
				continue
			}
		}
		if lotCost(in.RefPrice) > roomCap {
			reject(RuleLotUnaffordable, fmt.Sprintf("一手成本 %s 元超过本笔可用额度 %s 元",
				lotCost(in.RefPrice), roomCap))
			out = append(out, dec)
			continue
		}
		intent := in.WantFen
		if roomCap < intent {
			intent = roomCap
		}
		if st.Cash < intent {
			intent = st.Cash
		}
		qty := TargetQty(intent, in.RefPrice)
		amount := in.RefPrice.Mul(qty)
		if qty <= 0 || amount < minAmount {
			reject(RuleMinAmount, fmt.Sprintf("按现价只能投入 %s 元，低于生效单笔下限 %s 元",
				amount, minAmount))
			out = append(out, dec)
			continue
		}
		// 摩擦闸（20261103 定参①）：确定的成本必须小于不确定的预期收益。
		// ExpectedExcessPct 只能取归因的"买入 vs 跳过"差值，且**不显著时按 0**——
		// 0 意味着任何正摩擦都被拦，这是组长明确确认的设计意图（宁可长期空仓等
		// 样本与信号变强），不是缺陷。放在金额定稿之后：先算出真实名义额再估摩擦，
		// 否则用上限金额估会低估小额单的费率（最低佣金与名义额成反比）。
		if f := FrictionPct(amount); f >= m.P.ExpectedExcessPct {
			reject(RuleFrictionGate, fmt.Sprintf("摩擦闸：名义 %s 元的双边摩擦 %.2f%% ≥ 预估持有期超额 %.2f%%"+
				"（后者取不到显著归因值时按 0，即一律拦；回退键 signal.expected_excess_pct）",
				amount, f*100, m.P.ExpectedExcessPct*100))
			out = append(out, dec)
			continue
		}
		if amount > st.Cash {
			reject(RuleCashInsufficient, fmt.Sprintf("计划金额 %s 元超过可用现金 %s 元", amount, st.Cash))
			out = append(out, dec)
			continue
		}
		// 6) 批次累计总仓位（先斩后奏的历史 P0 bug 在此拦截）
		if committed+amount > totalCap {
			reject(RuleMaxTotalPosition, fmt.Sprintf("批次累计仓位 %d 分 + 本笔 %d 分 超过总仓位上限 %d 分（%.0f%%×总资产）",
				int64(committed), int64(amount), int64(totalCap), m.P.MaxTotalPositionPct*100))
			out = append(out, dec)
			continue
		}

		// 通过：累计在途，供后续笔核算。加仓不占新名额（posCount 只在开新仓时递增）。
		dec.Approved = true
		dec.Qty = qty
		dec.Amount = amount
		dec.PlannedPct = float64(amount) / float64(st.TotalAsset)
		committed += amount
		st.Cash -= amount
		if isAdd {
			st.HeldMV[in.TsCode] += amount
		} else {
			posCount++
			st.HeldCodes[in.TsCode] = true
			st.HeldMV[in.TsCode] = amount
			if !in.IsETF && ind != "" {
				industryCount[ind]++
			}
		}
		out = append(out, dec)
	}
	return out
}
