package llm

// 批量 prompt 的正文组装：一条 prompt 覆盖当日全部候选，一只票一段。
//
// 为什么写成一段一段而不是拼成一张大表：证据维度要看的字段各不一样，
// 表格会把"这一只票的检索任务"和"那一只票的冲击成本"混在一起，模型答串行成本低。
// 每段开头都用 ts_code + 名称，与要求回传的 results[].ts_code 一字不差。

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/signal"
)

// evidenceUser 一条证据 prompt 的整批正文。
func evidenceUser(p PromptSpec, items []signal.BuyRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, `【批次】本维度（%s）要评审 %d 只候选，逐只独立判断：`+
		"一只票的结论不受其他票影响，也不许拿它们互相凑理由。\n", p.Title, len(items))
	for i, it := range items {
		fmt.Fprintf(&b, "\n——— 第 %d/%d 只 ———\n%s%s", i+1, len(items),
			headerBlock(it), dimBlock(p.Key, it))
	}
	b.WriteString(evidenceTask(items))
	return b.String()
}

// decisionUser 决策 prompt 的整批正文：资金口径 + 每只票的四条评审结论。
func decisionUser(items []signal.BuyRequest, conclusions map[string][]string) string {
	var b strings.Builder
	b.WriteString(batchMoney(items))
	for i, it := range items {
		code := it.Candidate.TsCode
		fmt.Fprintf(&b, "\n——— 第 %d/%d 只：%s %s ———\n%s%s\n【评审结论】\n",
			i+1, len(items), code, it.Candidate.Name, headerBlock(it), priceBlock(it))
		for _, c := range conclusions[code] {
			b.WriteString(c + "\n")
		}
	}
	b.WriteString(decisionTask(items))
	return b.String()
}

// headerBlock 标的基本信息与选股漏斗给出的数（每只票每段都带一次）。
func headerBlock(it signal.BuyRequest) string {
	if it.Candidate.IsETF() {
		return etfHeaderBlock(it)
	}
	c := it.Candidate
	window := windowLine(it)
	base := fmt.Sprintf(
		"【标的】%s %s ｜ 行业：%s ｜ 收盘 %.2f 元 ｜ 流通市值 %.1f 亿 ｜ 换手率 %.2f%%\n"+
			"【估值】PE(TTM) %.1f ｜ PB %.2f\n"+
			"【选股漏斗】综合分 %.1f（动量 %.0f 价值 %.0f 低波 %.0f 流动性 %.0f）\n"+
			"　　↑ 这些分项是在 %d 只候选内部的截面百分位，不是绝对评价：基数这么小，"+
			"出现 0 分只说明它在这几只里排最后，不说明它在全市场差 —— 全市场的差票已在上一级被筛掉。\n"+
			"【漏斗给出的理由】%s\n【数据窗口】%s",
		c.TsCode, c.Name, c.Industry, c.Close.Float(), c.CircMvW/10000, c.TurnoverRate,
		c.PETtm, c.PB, c.Score, c.Factors.Momentum, c.Factors.Value, c.Factors.LowVol,
		c.Factors.Liquidity, c.PoolSize, clip(c.Reason, 120), window)
	return base + holdingBlock(it)
}

// windowLine 数据窗口那一行（个股与 ETF 共用）。
func windowLine(it signal.BuyRequest) string {
	window := fmt.Sprintf("截至 %s 收盘的最近 %d 个交易日", it.TradeDate, len(it.Bars.Closes))
	if !it.RulesOK {
		window += fmt.Sprintf("，但只有 %d 根，指标不足以支撑完整判断", len(it.Bars.Closes))
	}
	return window
}

// etfHeaderBlock 场内 ETF 的基本信息块。
//
// 与个股那一段的差别不是措辞而是**字段是否存在**：ETF 没有 PE/PB/流通市值/换手率，
// 也没有"行业"（它跟踪的是一篮子股票）。若照个股模板套，这几栏会被渲染成
// "PE(TTM) 0.0 ｜ PB 0.00"——那是把"没有数据"说成"数据为零"，模型会据此
// 判出"估值极低"这种结论。所以这里换一套只讲实话的字段，并把缺口明写出来。
func etfHeaderBlock(it signal.BuyRequest) string {
	c := it.Candidate
	return fmt.Sprintf(
		"【标的】%s %s ｜ 品种：场内 ETF ｜ 跟踪方向：%s\n"+
			"【成交规则】T+1 交割 ｜ 免印花税 ｜ 最小申报 100 份 ｜ 收盘 %.3f 元（本地按分取整，真实申报价以券商端为准）\n"+
			"【入围依据】%s\n"+
			"【没有的数据】无 PE / PB / 流通市值 / 换手率 / 个股级风险公告 —— 空白就是空白，不得当成 0 或低估值证据\n"+
			"【数据窗口】%s",
		c.TsCode, c.Name, c.Industry, c.Close.Float(), clip(c.Reason, 120), windowLine(it)) +
		holdingBlock(it)
}

// holdingBlock 已持有标的的持仓块（新仓返回空串）。模型据此做"加仓/维持"判断：
// 没有这段，模型根本不知道你手里有这只票，只能把加仓当新仓处理。
func holdingBlock(it signal.BuyRequest) string {
	h := it.Holding
	if h == nil || h.TotalQty <= 0 {
		return ""
	}
	cost, cur := h.CostPrice.Float(), it.Candidate.Close.Float()
	pnl := 0.0
	if h.CostPrice > 0 {
		pnl = (cur/cost - 1) * 100
	}
	unit, pfmt := "股", "%.2f"
	if it.Candidate.IsETF() {
		unit, pfmt = "份", "%.3f" // ETF 报价到厘：按两位显示会把 0.905 说成 0.90
	}
	return fmt.Sprintf(
		"\n【已持有】本标的已有持仓 %d %s ｜ 成本 %s 元 ｜ 现价 %s 元（浮盈亏 %+.1f%%）｜ 期间高点 %s 元\n"+
			"　　↑ 这是**加仓**判断：现有敞口 %.0f 元，单票上限内还能加 %.0f 元；"+
			"weight_pct 表示本次**追加**投入占总资产的比例（不是加完后的总仓位）。",
		int64(h.TotalQty), unit,
		fmt.Sprintf(pfmt, cost), fmt.Sprintf(pfmt, cur), pnl, fmt.Sprintf(pfmt, h.HighPrice.Float()),
		it.Budget.ExistingFen.Float(), it.Budget.AddRoomFen.Float())
}

// priceBlock 决策段里补一句价格：weight_pct 要落地成股数，模型得知道一手多少钱。
func priceBlock(it signal.BuyRequest) string {
	pfmt, unit := "%.2f", "股"
	if it.Candidate.IsETF() {
		pfmt, unit = "%.3f", "份"
	}
	return fmt.Sprintf("【成交口径】收盘 %s 元 ｜ 一手成本 %s 元（最小申报 100 %s）\n",
		fmt.Sprintf(pfmt, it.Candidate.Close.Float()), it.Budget.LotCostFen, unit)
}

// dimBlock 按维度取该票专属的证据块。
func dimBlock(key string, it signal.BuyRequest) string {
	switch key {
	case KeyTech:
		return techBlock(it)
	case KeyValue:
		return valueBlock(it)
	case KeyNews:
		return newsBlock(it)
	case KeySector:
		return sectorBlock(it)
	default:
		return ""
	}
}

// techBlock 技术形态证据块：规则算出的趋势/量能/形态数值。
func techBlock(it signal.BuyRequest) string {
	r := it.Rules
	return fmt.Sprintf(
		"\n【技术形态指标】MA5 %.2f 元 ｜ MA20 %.2f 元 ｜ MA5>MA20：%t\n"+
			"当日量/前5日均量 %.2f ｜ 区间涨幅 %+.1f%% ｜ 距窗口最高 %+.1f%% ｜ 日收益波动 %.1f%%",
		r.MA5, r.MA20, r.TrendUp, r.VolRatio, r.RetPct, r.OffHighPct, r.VolPct)
}

// valueBlock 估值证据块：明说这就是全部可得数据。
func valueBlock(it signal.BuyRequest) string {
	if it.Candidate.IsETF() {
		return etfValueBlock(it)
	}
	c := it.Candidate
	return fmt.Sprintf(
		"\n【估值全部可得数据】PE(TTM) %.1f ｜ PB %.2f ｜ 流通市值 %.1f 亿 ｜ 换手率 %.2f%% ｜ 所属行业 %s\n"+
			"（没有财务报表、没有盈利预测、没有行业景气数据 —— 这就是你手上的全部）",
		c.PETtm, c.PB, c.CircMvW/10000, c.TurnoverRate, c.Industry)
}

// etfValueBlock ETF 的"估值"维度：这个维度**没有数据**，如实告诉模型并要求它承认。
//
// 为什么不在这里塞替代指标：动量/波动在技术维度已经给过了，把它们换个标题写到估值段
// 只会诱导模型编出"ETF 分散持股所以估值风险低"这类无据结论。评审的 JSON 契约里有
// unknown 这一档，本块的作用就是让它成为诚实且唯一正确的答案。
func etfValueBlock(it signal.BuyRequest) string {
	return fmt.Sprintf(
		"\n【估值可得数据】无 —— ETF 是个股的一篮子集合，本系统没有它的 NAV、成分股与盈利数据，"+
			"个股口径的 PE/PB 对它不成立\n"+
			"　　↑ 跟踪方向：%s。本维度请如实回答 unknown=true，"+
			"不得把\"ETF 天生分散\"或白名单身份本身当成估值优势证据。\n", it.Candidate.Industry)
}

// newsBlock 消息面证据块：价格异常线索 + 这一只票的检索任务。
func newsBlock(it signal.BuyRequest) string {
	r, c := it.Rules, it.Candidate
	if c.IsETF() {
		return fmt.Sprintf(
			"\n【价格异常证据（本地算的，仅作辅助线索）】窗口内单日最大跌幅 %+.1f%% ｜ 放量下跌天数 %d ｜ 距窗口最高 %+.1f%%"+
				"（ETF 无涨跌停板概念，\"近似跌停次数\"一项对它不适用，故省略）\n"+
				"【本只的检索任务】查 \"%s %s\"：跟踪指数成分调整、份额折算、管理费调整、清盘/扩募公告、成交活跃度变化、跟踪误差。\n"+
				"　　↑ ETF 不承担个股那种立案调查/退市风险警示/业绩预亏/质押爆仓风险 —— 拿这些去否决 ETF 是评错了对象。\n",
			r.MaxDropPct, r.VolDownDays, r.OffHighPct, c.Name, shortCode(c.TsCode))
	}
	return fmt.Sprintf(
		"\n【价格异常证据（本地算的，仅作辅助线索）】窗口内近似跌停次数 %d ｜ 单日最大跌幅 %+.1f%% ｜ 放量下跌天数 %d ｜ 距窗口最高 %+.1f%%\n"+
			"【本只的检索任务】查 \"%s %s 风险公告\"：立案调查、退市风险警示、业绩预亏、监管处罚、质押爆仓。",
		r.LimitDownDays, r.MaxDropPct, r.VolDownDays, r.OffHighPct, c.Name, shortCode(c.TsCode))
}

// sectorBlock 板块与冲击成本证据块。
func sectorBlock(it signal.BuyRequest) string {
	c, r := it.Candidate, it.Rules
	if c.IsETF() {
		// ETF 没有"板块加权动量"与"流通市值"：本漏斗不排名，也就没算过这两栏，
		// 塞 0 进去会被读成"板块动量 0.0%（持平）"这种无中生有的结论。
		return fmt.Sprintf(
			"\n【跟踪方向】%s —— 白名单内不排名，与同批其他 ETF 没有高低之分，"+
				"不许拿\"它排第几名\"当理由\n"+
				"【自身动量】窗口区间动量 %+.1f%%\n"+
				"【冲击成本】窗口日均成交额 %.0f 元（已按流动性下限筛过）｜ 本次单票上限 %s 元 ｜ 一手成本 %s 元\n"+
				"　　↑ 场内 ETF 无个股冲击成本问题，但一只日成交不足 2 亿的窄基 ETF 卖出时同样要有让步空间。\n",
			c.Industry, c.Mom*100, r.AvgAmtYuan, it.Budget.SlotFen, it.Budget.LotCostFen)
	}
	return fmt.Sprintf(
		"\n【板块】行业 %s ｜ 板块20日加权动量 %+.1f%% ｜ 本票区间动量 %+.1f%%（相对板块 %+.1f 个点）\n"+
			"【冲击成本】窗口日均成交额 %.0f 元 ｜ 本票流通市值 %.1f 亿 ｜ 本次单票上限 %s 元 ｜ 一手成本 %s 元",
		c.Industry, c.SectorMom*100, c.Mom*100, (c.Mom-c.SectorMom)*100,
		r.AvgAmtYuan, c.CircMvW/10000, it.Budget.SlotFen, it.Budget.LotCostFen)
}

// batchMoney 整批共用的资金口径（同一天所有候选面对的是同一个账户）。
func batchMoney(items []signal.BuyRequest) string {
	b := items[0].Budget
	return fmt.Sprintf(
		"【账户口径】可用现金 %s 元 ｜ 单票上限 %s 元 ｜ 当前持仓 %d/%d 只（最多还能开 %d 只）\n"+
			"【本批候选】%d 只，一次裁决全部门户：weight_pct 是拟投入占总资产的比例，"+
			"全批投入之和不要超过可用现金。\n",
		b.CashFen, b.SlotFen, b.Positions, b.MaxPos, slotsLeft(b), len(items))
}

// slotsLeft 还能开几只新仓（负数按 0 表达：超仓时不该给模型看负名额）。
func slotsLeft(b signal.BuyBudget) int {
	if n := b.MaxPos - b.Positions; n > 0 {
		return n
	}
	return 0
}

// evidenceTask 证据 prompt 统一的输出契约（含必须照抄的代码清单）。
func evidenceTask(items []signal.BuyRequest) string {
	return "\n请只输出一个 JSON 对象，不要解释文字、不要代码围栏：\n" +
		`{"results":[{"ts_code":"代码","stance":"positive|neutral|negative|unknown",` +
		`"finding":"不超过60字的一句话","unknown":true或false}]}` +
		"\nts_code 必须逐字照抄这些：" + codesList(items) +
		fmt.Sprintf("\n一共 %d 只，一只都不能漏，也不许新增没给给你的标的。\n", len(items))
}

// decisionTask 决策 prompt 的输出契约。
func decisionTask(items []signal.BuyRequest) string {
	return "\n请只输出一个 JSON 对象，不要解释、不要代码围栏：\n" +
		`{"results":[{"ts_code":"代码","approve":true或false,"weight_pct":0到1的小数,` +
		`"confidence":0到1的小数,"reason":"不超过80字，点名至少两条你采信的具体证据"}]}` +
		"\nts_code 必须逐字照抄这些：" + codesList(items) +
		fmt.Sprintf("\n一共 %d 只，每只都要给结论。\n", len(items))
}

// codesList 用 ts_code 原样拼出的清单（模型回传时要一字不差）。
func codesList(items []signal.BuyRequest) string {
	codes := make([]string, 0, len(items))
	for _, it := range items {
		codes = append(codes, it.Candidate.TsCode)
	}
	return strings.Join(codes, "、")
}
