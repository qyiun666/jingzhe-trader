package risk

import (
	"fmt"

	"jingzhe-trader/internal/model"
)

// 否决规则名（落 signal.reject_rule，验收 #7/#8 的取值契约）。
const (
	RuleAllowNewPosition = "allow_new_position"
	RuleMinConfidence    = "min_confidence"
	RuleScoreThreshold   = "score_threshold"
	RuleMaxPositions     = "max_positions"
	RuleAlreadyHolding   = "already_holding"
	RuleMaxPosition      = "max_position"
	RuleMinAmount        = "min_amount"
	RuleCashInsufficient = "cash_insufficient"
	RuleMaxTotalPosition = "max_total_position"
	RuleIllegalPrice     = "illegal_price"
)

// AccountState 风控核算的账户状态快照（金额一律分）。
type AccountState struct {
	TotalAsset    model.Fen      // 总资产
	Cash          model.Fen      // 可用现金
	PositionsMV   model.Fen      // 持仓市值（成本或最新价口径，由调用方决定）
	PositionCount int            // 当前持仓只数
	HeldCodes     map[string]bool // 已持有代码集合
}

// BuyIntent 单笔买入意向（来自通过规则筛选的买入信号）。
type BuyIntent struct {
	TsCode     string
	Name       string
	RefPrice   model.Fen // 参考价（分）
	Confidence float64   // 信号置信度 0~1
	Score      float64   // 因子综合分 0~100
}

// Decision 批次风控裁决结果。
type Decision struct {
	TsCode     string
	Approved   bool
	RejectRule string // 否决时非空（100% 留痕，禁静默丢弃）
	RejectMsg  string
	Qty        model.Qty   // 通过时给出的建议股数（整手）
	Amount     model.Fen   // 通过时的计划金额（分）
	PlannedPct float64     // 计划金额占总资产比例
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
	singleCap := pctOf(st.TotalAsset, m.P.MaxPositionPct)
	if cap2 := pctOf(st.TotalAsset, m.P.MaxSingleAmountPct); cap2 < singleCap {
		singleCap = cap2
	}
	minAmount := m.P.MinSingleAmountFen

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
		// 2) 置信度与评分门槛
		if in.Confidence < m.P.MinConfidence {
			reject(RuleMinConfidence, fmt.Sprintf("置信度 %.2f 低于下限 %.2f", in.Confidence, m.P.MinConfidence))
			out = append(out, dec)
			continue
		}
		if in.Score < 60*m.P.ScoreThresholdMul {
			reject(RuleScoreThreshold, fmt.Sprintf("综合分 %.1f 低于门槛 %.1f（60×%.2f）",
				in.Score, 60*m.P.ScoreThresholdMul, m.P.ScoreThresholdMul))
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
		// 4) 价格合法性与单笔金额核算
		if in.RefPrice <= 0 {
			reject(RuleIllegalPrice, fmt.Sprintf("参考价非法: %d 分", int64(in.RefPrice)))
			out = append(out, dec)
			continue
		}
		intent := singleCap
		if st.Cash < intent {
			intent = st.Cash
		}
		qty := TargetQty(intent, in.RefPrice)
		amount := in.RefPrice.Mul(qty)
		if qty <= 0 || amount < minAmount {
			reject(RuleMinAmount, fmt.Sprintf("单笔金额 %d 分（价格 %d 分）低于下限 %d 分",
				int64(amount), int64(in.RefPrice), int64(minAmount)))
			out = append(out, dec)
			continue
		}
		if amount > st.Cash {
			reject(RuleCashInsufficient, fmt.Sprintf("计划金额 %d 分超过可用现金 %d 分", int64(amount), int64(st.Cash)))
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
