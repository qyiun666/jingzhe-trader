package risk

import (
	"fmt"

	"jingzhe-trader/internal/model"
)

// 否决规则名（落 order_ticket 记录与日志，验收 #7/#8 的取值契约）。
const (
	RuleAllowNewPosition = "allow_new_position"
	RuleMinConfidence    = "min_confidence"
	RuleMaxPositions     = "max_positions"
	RuleAlreadyHolding   = "already_holding"
	RuleLotUnaffordable  = "lot_unaffordable"
	RuleMinAmount        = "min_amount"
	RuleCashInsufficient = "cash_insufficient"
	RuleMaxTotalPosition = "max_total_position"
	RuleIllegalPrice     = "illegal_price"
)

// AccountState 风控核算的账户状态快照（金额一律分）。
type AccountState struct {
	TotalAsset    model.Fen       // 总资产
	Cash          model.Fen       // 可用现金
	PositionsMV   model.Fen       // 持仓市值（成本或最新价口径，由调用方决定）
	PositionCount int             // 当前持仓只数
	HeldCodes     map[string]bool // 已持有代码集合
}

// BuyIntent 单笔买入意向：决策链给出"买这只、想要这么多钱"，Manager 负责砍到硬上限之内。
type BuyIntent struct {
	TsCode     string
	Name       string
	RefPrice   model.Fen // 参考价（分）
	Confidence float64   // 决策自报置信度 0~1
	WantFen    model.Fen // 期望投入金额（分）；超过单票上限会被截断
}

// Decision 批次风控裁决结果。
type Decision struct {
	TsCode     string
	Approved   bool
	RejectRule string // 否决时非空（100% 留痕，禁静默丢弃）
	RejectMsg  string
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

	for _, in := range intents {
		dec := Decision{TsCode: in.TsCode}
		reject := func(rule, msg string) {
			dec.Approved = false
			dec.RejectRule = rule
			dec.RejectMsg = msg
		}

		// 1) 档位开关
		if !m.P.AllowNewPosition || m.P.Bias == BiasExitOnly {
			reject(RuleAllowNewPosition, fmt.Sprintf("当前档位 %s 禁止开新仓", m.P.Bias))
			out = append(out, dec)
			continue
		}
		// 2) 决策置信度下限（决策方自报值，风控保留的唯一质量门槛）
		if in.Confidence < m.P.MinConfidence {
			reject(RuleMinConfidence, fmt.Sprintf("决策置信度 %.2f 低于档位下限 %.2f", in.Confidence, m.P.MinConfidence))
			out = append(out, dec)
			continue
		}
		// 3) 持仓数与重复持仓
		if st.HeldCodes[in.TsCode] {
			reject(RuleAlreadyHolding, "已持有该标的，不重复建仓")
			out = append(out, dec)
			continue
		}
		if posCount >= m.P.MaxPositions {
			reject(RuleMaxPositions, fmt.Sprintf("持仓数 %d（含本批在途）达到上限 %d", posCount, m.P.MaxPositions))
			out = append(out, dec)
			continue
		}
		// 4) 价格合法性与金额核算：以决策要求的金额为准，被单票上限与现金双重截断
		if in.RefPrice <= 0 {
			reject(RuleIllegalPrice, fmt.Sprintf("参考价非法: %d 分", int64(in.RefPrice)))
			out = append(out, dec)
			continue
		}
		if lotCost(in.RefPrice) > singleCap {
			reject(RuleLotUnaffordable, fmt.Sprintf("一手成本 %s 元超过单票上限 %s 元",
				lotCost(in.RefPrice), singleCap))
			out = append(out, dec)
			continue
		}
		intent := in.WantFen
		if singleCap < intent {
			intent = singleCap
		}
		if st.Cash < intent {
			intent = st.Cash
		}
		qty := TargetQty(intent, in.RefPrice)
		amount := in.RefPrice.Mul(qty)
		if qty <= 0 || amount < minAmount {
			reject(RuleMinAmount, fmt.Sprintf("按现价只能投入 %s 元，低于本档生效单笔下限 %s 元",
				amount, minAmount))
			out = append(out, dec)
			continue
		}
		if amount > st.Cash {
			reject(RuleCashInsufficient, fmt.Sprintf("计划金额 %s 元超过可用现金 %s 元", amount, st.Cash))
			out = append(out, dec)
			continue
		}
		// 5) 批次累计总仓位（先斩后奏的历史 P0 bug 在此拦截）
		if committed+amount > totalCap {
			reject(RuleMaxTotalPosition, fmt.Sprintf("批次累计仓位 %d 分 + 本笔 %d 分 超过总仓位上限 %d 分（%.0f%%×总资产）",
				int64(committed), int64(amount), int64(totalCap), m.P.MaxTotalPositionPct*100))
			out = append(out, dec)
			continue
		}

		// 通过：累计在途，供后续笔核算
		dec.Approved = true
		dec.Qty = qty
		dec.Amount = amount
		dec.PlannedPct = float64(amount) / float64(st.TotalAsset)
		committed += amount
		st.Cash -= amount
		posCount++
		out = append(out, dec)
	}
	return out
}
