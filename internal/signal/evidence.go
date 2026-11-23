package signal

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// RuleEvidence 规则算出的证据列。
//
// 用户拍板：买卖决策交给 LLM，规则信号（原 ma_cross 等）降级为喂进 prompt 的证据，
// 不再自己产指令 —— 本结构因此没有任何"是否通过"字段，只有数值。
// 其中后五项（MaxDropPct / LimitDownDays / VolDownDays / OffHighPct / VolPct）是
// 消息面 prompt 的唯一素材：接口拿不到新闻，只能用价格异常代替，且必须让模型知道这一点。
type RuleEvidence struct {
	TrendUp       bool    // MA5 > MA20
	MA5           float64 // 前复权收盘（元）
	MA20          float64
	VolRatio      float64 // 当日量 / 前 5 日均量
	RetPct        float64 // 窗口区间涨幅（%）
	OffHighPct    float64 // 距窗口最高收盘（%，≤0）
	VolPct        float64 // 日收益率标准差（%）
	MaxDropPct    float64 // 窗口内单日最大跌幅（%，≤0）
	LimitDownDays int     // 近似跌停天数（单日 ≤ -9.8%）
	VolDownDays   int     // 放量下跌天数（量 > 1.5× 均量且收跌）
	AvgAmtYuan    float64 // 窗口日均成交额（元）
}

// minEvidenceBars 证据计算所需的最少根数（与 MA20 同口径）。
const minEvidenceBars = 20

// nearLimitDownPct 近似跌停阈值：主板 10%，留 0.2% 容差；创业/科创 20% 无法由此判定（宁可漏判不误判）。
const nearLimitDownPct = -9.8

// EvaluateRules 由行情窗口计算证据列。数据不足 minEvidenceBars 根时 ok=false，
// 调用方必须把"证据不全"显式告诉 LLM，而不是拿部分窗口当完整窗口评审。
func EvaluateRules(bs BarSeries) (e RuleEvidence, ok bool) {
	if len(bs.Closes) < minEvidenceBars || len(bs.Vols) < minEvidenceBars {
		return e, false
	}
	e.MA5, e.MA20 = MA5MA20(bs.Closes)
	e.TrendUp = e.MA5 > e.MA20
	e.VolRatio = VolumeRatio(bs.Vols)

	first, last := bs.Closes[0], bs.Closes[len(bs.Closes)-1]
	e.RetPct = pctChange(last, first)
	var high float64
	var rets []float64
	var prev float64
	var amtSum float64
	avgVol := mean(bs.Vols[:len(bs.Vols)-1]) // 均量不含当日，避免自比
	for i, c := range bs.Closes {
		if c > high {
			high = c
		}
		if i > 0 {
			rets = append(rets, pctChange(c, prev))
		}
		prev = c
		if i < len(bs.Raws) {
			amtSum += bs.Raws[i] * bs.Vols[i] // 分 × 手 = 元（100 与 1/100 抵消）
		}
	}
	e.OffHighPct = pctChange(last, high)
	e.VolPct = stdevPct(rets)
	e.AvgAmtYuan = amtSum / float64(len(bs.Closes))
	for _, r := range rets {
		if r < e.MaxDropPct {
			e.MaxDropPct = r
		}
	}
	for i, r := range rets {
		if r <= nearLimitDownPct {
			e.LimitDownDays++
		}
		// rets[i] 对应 bs.Closes[i+1] / bs.Vols[i+1]：放量下跌 = 量 > 1.5× 均量且收跌
		if r < 0 && avgVol > 0 && bs.Vols[i+1] > 1.5*avgVol {
			e.VolDownDays++
		}
	}
	return e, true
}

// pctChange (now-prev)/prev×100；prev ≤ 0 返回 0（停牌/异常 0 价不参与）。
func pctChange(now, prev float64) float64 {
	if prev <= 0 {
		return 0
	}
	return (now - prev) / prev * 100
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// stdevPct 样本标准差（对已经是百分数的收益率）。
func stdevPct(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	var sq float64
	for _, x := range xs {
		sq += (x - m) * (x - m)
	}
	return math.Sqrt(sq / float64(len(xs)-1))
}

// ===== 涨跌停可成交性判定（阶段1 影子，无条件观测）=====
//
// 判定器只消费交易所公布的涨停价/跌停价，内部不出现任何涨跌幅阈值：
// 主板/ST/创业板/科创板/北交所的差异完全由档位价格承载，判定器不看板块。

// LimitDay 单日判定所需的最小行情面（分，真实成交价口径）。
// 零值字段即表示无值，由 store 侧以缺失键 map 交付，不得填 0 冒充有效价。
type LimitDay struct {
	Up       model.Fen // 涨停价；≤0 = 无值
	Down     model.Fen // 跌停价；≤0 = 无值
	RawClose model.Fen // 判定唯一价格基准（真实收盘价）
	High     model.Fen // 0 = 未落库，只影响封板形态，不影响四态
	Low      model.Fen
}

// LimitState 四态。载荷与归因只认这四个字面量。
type LimitState string

const (
	LimitTradeable   LimitState = "tradeable"
	LimitBlockedUp   LimitState = "blocked_limit_up"
	LimitBlockedDown LimitState = "blocked_limit_down"
	LimitUnknown     LimitState = "unknown"
)

// LimitSeal 封板形态，仅 blocked_* 有意义。一字封死与收盘封板必须不同名。
type LimitSeal string

const (
	SealNone       LimitSeal = ""           // 非封板态
	SealOneWord    LimitSeal = "one_word"   // 一字封死（high==low）
	SealCloseOnly  LimitSeal = "close_only" // 收盘触及，盘中曾离开档位
	SealUnresolved LimitSeal = "unresolved" // high/low 缺失，形态不可判
)

// LimitReason reason 字段的唯一来源：unknown 用前四个，次日回收未成交用 sealed_one_word。
type LimitReason string

const (
	ReasonNoRow    LimitReason = "no_row"          // 当日无行情行（由调用方构造）
	ReasonLimitLE0 LimitReason = "limit_le0"       // 档位价缺失或非正
	ReasonInverted LimitReason = "inverted"        // 涨停价 < 跌停价（数据异常）
	ReasonNoPrice  LimitReason = "no_price"        // 判定基准价缺失
	ReasonSealed   LimitReason = "sealed_one_word" // 次日一字封死，影子单未回收成（非数据未知）
)

// LimitVerdict 判定结果。
type LimitVerdict struct {
	State   LimitState
	Reason  LimitReason
	Seal    LimitSeal
	Anomaly bool // 价格越过交易所公布档位：按同向封板处理，同时留数据异常标记
}

// Blocks 该判定是否拦截给定方向。unknown 不拦截——观测面不得改变实盘决策。
func (v LimitVerdict) Blocks(d model.Direction) bool {
	switch v.State {
	case LimitBlockedUp:
		return d == model.DirBuy
	case LimitBlockedDown:
		return d == model.DirSell
	default:
		return false
	}
}

// UnknownNoRow 供调用方在 store 的缺失键 map 未命中时构造，reason 字面量不在调用方重复一份。
func UnknownNoRow() LimitVerdict { return LimitVerdict{State: LimitUnknown, Reason: ReasonNoRow} }

// limitPairErr 档位对是否可用，返回空串表示可用。判定与回收共用一份条件，避免两处漂移。
func limitPairErr(up, down model.Fen) LimitReason {
	switch {
	case up <= 0 || down <= 0:
		return ReasonLimitLE0
	case up < down:
		return ReasonInverted
	}
	return ""
}

// JudgeLimitDay 判定单日收盘档可成交性。无 IO、无随机、无时间依赖。
func JudgeLimitDay(px LimitDay) LimitVerdict {
	if r := limitPairErr(px.Up, px.Down); r != "" {
		return LimitVerdict{State: LimitUnknown, Reason: r}
	}
	switch {
	case px.RawClose <= 0:
		return LimitVerdict{State: LimitUnknown, Reason: ReasonNoPrice}
	case px.RawClose >= px.Up:
		v := LimitVerdict{State: LimitBlockedUp, Seal: sealOf(px)}
		v.Anomaly = px.RawClose > px.Up
		return v
	case px.RawClose <= px.Down:
		v := LimitVerdict{State: LimitBlockedDown, Seal: sealOf(px)}
		v.Anomaly = px.RawClose < px.Down
		return v
	}
	return LimitVerdict{State: LimitTradeable}
}

// sealOf 封板形态。high==low 记一字；本仓无停牌列，全天停牌且收在档位上会被判为一字，
// 这是阶段1 已声明的限制。
func sealOf(px LimitDay) LimitSeal {
	if px.High <= 0 || px.Low <= 0 {
		return SealUnresolved
	}
	if px.High == px.Low {
		return SealOneWord
	}
	return SealCloseOnly
}

// T1Quote 回收 T 日影子单所需的次日行情（分，真实成交价口径）。
type T1Quote struct {
	Open     model.Fen
	High     model.Fen
	Low      model.Fen
	RawClose model.Fen
	Up       model.Fen
	Down     model.Fen
}

// T1Outcome 回收结果。理论盈亏不落库、每天用当日数据重算，故不引入任何状态字段。
type T1Outcome struct {
	Fillable bool
	Entry    model.Fen // 可成交时的次日成交基准价（开盘价）
	Exit     model.Fen // 次日收盘（真实成交价）
	RetPct   float64   // 复用 pctChange，唯一涨幅算式来源
	Reason   LimitReason
	Anomaly  bool // 档位缺失/越界/缺价
}

// RecoverT1 用次日实际行情回收一张影子单，只用交易所档位：
// 买向全天最低价不离开涨停价 ⇒ 一字涨停买不到；卖向最高价不离开跌停价 ⇒ 一字跌停卖不掉；
// 其余按次日开盘价可成交（保守近似，不含盘中排队）。high/low 缺失时不判一字。
// 本仓无 pre_close 列，除权日的 RetPct 会失真且不记异常 —— 阶段1 已声明的限制。
func RecoverT1(d model.Direction, q T1Quote) T1Outcome {
	if r := limitPairErr(q.Up, q.Down); r != "" {
		return T1Outcome{Reason: r, Anomaly: true}
	}
	if q.Open <= 0 || q.RawClose <= 0 {
		return T1Outcome{Reason: ReasonNoPrice, Anomaly: true}
	}
	// 两支对缺失值的处理刻意不对称：买向 Low=0 时 Low>=Up 恒假（Up>0 已由 limitPairErr 保证），
	// 天然不判一字；卖向 High=0 时 High<=Down 会假成立，必须显式守卫，否则缺数据被当成一字跌停。
	blocked := d == model.DirBuy && q.Low >= q.Up
	blocked = blocked || (d == model.DirSell && q.High > 0 && q.High <= q.Down)
	if blocked {
		anom := (q.Low > 0 && q.Low > q.Up) || (q.High > 0 && q.High < q.Down)
		return T1Outcome{Reason: ReasonSealed, Anomaly: anom}
	}
	out := T1Outcome{Fillable: true, Entry: q.Open, Exit: q.RawClose}
	out.RetPct = pctChange(q.RawClose.Float(), q.Open.Float())
	out.Anomaly = q.Open > q.Up || q.Open < q.Down
	return out
}

// ===== limit_brief 影子摘要（裁定③⑤⑥：只附加进 screen:topn 载荷末尾，不进任何决策）=====

// limitBriefTop 摘要最多列出的只数（与板块影子先例 sectorShadowTop 一致）。
const limitBriefTop = 10

// limitBriefMax 摘要整段长度上限，按字符数计（不是字节数——中文 3 字节），同 sectorShadowBriefMax。
const limitBriefMax = 600

// limitBriefBasis 口径句，只此一份：空集句、清单句、只报总数的兜底句都从这里拼，不各写一遍。
const limitBriefBasis = "档位取交易所公布涨跌停价，价格基准=真实收盘价，纯观测不参与决策"

// limitTally 一次遍历数出来的四档计数；total 由计数相加得出，不作为第二份真相保存。
type limitTally struct{ cands, up, down, unknown int }

func (t limitTally) total() int { return t.up + t.down + t.unknown }

// head 拼「涨跌停影子 N/候选 M 只（拦买 a、拦卖 b、判定未知 c；口径[，列前 n]」，不含收尾括号。
// n<=0 表示"不列条目"（0 拦截句与只报总数的兜底句），此时不提列前只数。
func (t limitTally) head(n int) string {
	s := fmt.Sprintf("涨跌停影子 %d/候选 %d 只（拦买 %d、拦卖 %d、判定未知 %d；%s", t.total(), t.cands, t.up, t.down, t.unknown, limitBriefBasis)
	if n > 0 && t.total() > n {
		s += fmt.Sprintf("，列前 %d", n)
	}
	return s
}

// LimitBrief 按候选顺序逐只判定，产出一句涨跌停影子摘要。
//
// rows 必须是缺失键 map：未命中即无键 ⇒ 记 no_row。不许用零值结构体顶替，那样只能得到
// limit_le0，而「交易所没供这个档位的数」和「库里没这一行」在归因上是两回事。
//
// 键的有无只区分「算过 / 旧二进制」：无条件观测下只要跑过就必出句子，当日 0 拦截也写
// 「涨跌停影子 0/候选 M 只」——omitempty 对空串同样省略，所以这里绝不能返回空串。
func LimitBrief(codes []string, rows map[string]LimitDay) string {
	var t limitTally
	t.cands = len(codes)
	var entries []string
	for _, code := range codes {
		px, ok := rows[code]
		v := UnknownNoRow()
		if ok {
			v = JudgeLimitDay(px)
		}
		switch v.State {
		case LimitBlockedUp:
			t.up++
		case LimitBlockedDown:
			t.down++
		case LimitUnknown:
			t.unknown++
		case LimitTradeable:
			continue
		}
		entries = append(entries, limitEntry(code, v, px))
	}
	if t.total() == 0 {
		return t.head(0) + "）：当日候选无一触板，判定已生效。"
	}
	for n := min(limitBriefTop, len(entries)); n >= 1; n-- {
		brief := t.head(n) + "）：" + strings.Join(entries[:n], "、") + "。"
		if utf8.RuneCountInString(brief) <= limitBriefMax {
			return brief
		}
	}
	// 连一只都放不下（代码或档位异常长）：至少把总数与四档计数报全，不给半条记录。
	return t.head(0) + "，条目过长只报总数）。"
}

// limitEntry 一条记录：代码 状态[/形态][/原因] 触发价（元，走 model.Fen.String 这唯一展示口）。
// 状态与原因直接写四态字面量：归因与 grep 用的就是这些串，翻成中文等于再造一份口径。
func limitEntry(code string, v LimitVerdict, px LimitDay) string {
	s := code + " " + string(v.State)
	if v.Seal != SealNone {
		s += "/" + string(v.Seal)
	}
	if v.Reason != "" {
		s += "/" + string(v.Reason)
	}
	if px.RawClose > 0 {
		s += " " + px.RawClose.String()
	}
	return s
}

// ===== 数据面适配器（裁定【1】：store 不得反向依赖 signal，转换写在这一侧）=====

// fenOrZero 把库里的三态（非 nil=有值 / nil=NULL=没这个档位）转成 LimitDay 的「无值=0」表示。
// nil 不等于 0 元：0 在这里只表示「没有这个数」，判定器据此落 unknown/limit_le0。
func fenOrZero(p *model.Fen) model.Fen {
	if p == nil {
		return 0
	}
	return *p
}

// LimitDays 把 store.LimitRowsFor 的封板价格截面转成判定输入。
//
// 只搬值、不补键：库里当日没有某只票的行 ⇒ 那个键在结果 map 里也不出现，摘要据此记
// no_row；这里若给缺失的票造一个零值条目，就把「交易所没供数」压成了「档位是 0」（裁定【1】）。
//
// store.LimitRow 只有 ts_code/raw_close/up_limit/down_limit 四列，不带 high/low ⇒
// 封板形态一律 unresolved：阶段1 的摘要能分「触没触板」，分不出「一字还是收盘才封」，
// 这条限制在 11-23 handoff §四 已具名报出，要打通得由组长裁定给 LimitRow 增列。
func LimitDays(rows map[string]store.LimitRow) map[string]LimitDay {
	out := make(map[string]LimitDay, len(rows))
	for code, r := range rows {
		out[code] = LimitDay{Up: fenOrZero(r.UpLimit), Down: fenOrZero(r.DownLimit), RawClose: r.RawClose}
	}
	return out
}
