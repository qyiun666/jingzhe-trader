// Package screener 选股：一条顺序流水线（板块 → 可用资金 → 流动性 → 估值 → 因子排名）。
//
// 设计要点：
//   - 过程不落库：候选与淘汰计数只在内存里传递，每级结果写日志，
//     整条链路唯一落库的产出是 order_ticket；
//   - 板块是纯硬门槛、不补位（用户拍定）：跌出 Top K 板块的成员当日不参与打分，
//     0 候选是合法结果，卖出链路不受板块门槛影响；
//   - 板块强弱自算：申万行业指数日线在 500 积分档无权限（实测返回 0 行），
//     因此用本地 daily_bar 按 stock_basic.industry 聚合，零新增接口调用；
//   - 因子克制且可解释：动量 / 价值 / 低波 / 流动性 4 项截面百分位加权。
//     质量（ROE）因子已废——财务表实测只覆盖 20/5553 只，基本面判断改由 LLM 决策承担。
//
// 依赖方向：screener 只依赖 model 与 store，不触网（数据全部来自 SQLite 缓存）。
package screener

import (
	"time"

	"jingzhe-trader/internal/market"

	"jingzhe-trader/internal/model"
)

// FilterConfig 漏斗参数（全部可配，来源 config screen.* 键）。
type FilterConfig struct {
	TopN             int     // 候选池容量
	BufferN          int     // 卖出规则 4 的排名保留带深度（signal.rank_out_buffer_n）：保留带 = Top(TopN+BufferN)，0 = 与 TopN 等深（旧口径）
	MinCircMvW       float64 // 流通市值下限（万元）
	MinTurnoverRate  float64 // 换手率下限（%）
	PriceLow         float64 // 价格下限（元，剔除仙股）
	PETtmMax         float64 // PE_TTM 上限（亏损股剔除）
	PBMax            float64 // PB 上限
	MinListDays      int     // 上市天数下限（新股剔除）
	SectorTopK       int     // 板块强弱保留前 K 个行业
	MinSectorMembers int     // 行业成员数下限（不足则该行业不参与排名）
	// MaxPerIndustry 因子 TopN 每行业最多几只（0=关闭）。保留带按同一上限选，
	// 保证"候选一定是保留带成员"——否则买入当晚就可能因排名在半带外被规则 4 淘汰。
	MaxPerIndustry int
}

// 漏斗各级淘汰原因（统一文案，只出现在日志与告警正文）。
const (
	reasonST             = "ST/风险警示"
	reasonNewStock       = "上市不足期"
	reasonSuspended      = "当日停牌"
	reasonNoIndustry     = "无行业归属"
	reasonSectorOut      = "非强势板块"
	reasonNoValuation    = "估值截面缺失（数据未同步）"
	reasonUnaffordable   = "一手超单笔预算"
	reasonSmallMV        = "流通市值过小"
	reasonIlliquid       = "换手率不足"
	reasonPriceOut       = "价格过低"
	reasonBadPE          = "亏损或PE超标"
	reasonBadPB          = "PB超标"
	reasonRankOut        = "排名不足TopN"
	reasonIndustryCap    = "行业集中度超限"
	reasonMarketRegime   = "大盘跌破%s，当日关闭买入漏斗" // %s 由 Report.RegimeMALabel() 填充
	reasonSectorNoRank   = "板块动量数据不足"
	minSectorDataMembers = 10 // 板块内可算动量的成员不足则放弃该板块排名
)

// basicEligible 基础资格：非 ST、上市满 minListDays、当日未停牌。
// 返回是否通过与淘汰原因（通过时为空串）。
func basicEligible(s model.StockBasic, listDays, minListDays int, suspended bool) (bool, string) {
	if market.IsSTName(s.Name) {
		return false, reasonST
	}
	if s.ListStatus != "" && s.ListStatus != "L" {
		return false, reasonST // 退市/摘牌归并到风险警示类
	}
	if listDays < 0 || listDays < minListDays { // listDays<0 表示解析失败，宁缺勿滥
		return false, reasonNewStock
	}
	if suspended {
		return false, reasonSuspended
	}
	return true, ""
}

// sectorGateStage 板块硬门槛：只保留强势板块 Top K 的成员。
func sectorGateStage(industry string, hot map[string]bool) (bool, string) {
	if industry == "" {
		return false, reasonNoIndustry
	}
	if !hot[industry] {
		return false, reasonSectorOut
	}
	return true, ""
}

// lotsPerShare A 股一手 = 100 股。
const lotsPerShare = 100

// reasonLotWindow 整手金额落不进[单笔下限, 单票上限]窗口（20261104 策略 B1）。
const reasonLotWindow = "整手金额落不进单笔下限与单票上限之间"

// affordableStage 可用资金筛：一手成本必须落在**风控真的能下单**的金额区间内。
//
// 两个边界都必须判，只判上界是缺陷（B1 修正前的形态）：
//   - 上界 capFen = 单票上限（Budget.perSlot()，与风控 SingleCapFen 同一条线）；
//   - 下界 minFen = 风控生效的单笔下限（risk.MinAmountFloor 的口径，由调用方注入）。
//     风控在金额定稿后还会判 `amount < minAmount → 否决`（RuleMinAmount），而申报量
//     必须是 100 股的整数倍：窗口 [880.34, 1,173.78] 只有 293 元宽，而整手步距是
//     100×price。11-03 实测 20 只候选里 13 只**没有任何一手倍数能落进这个窗口**
//     （上海银行一手 833 < 下限、两手 1,666 > 上限），它们照过了漏斗、每天白过一遍
//     LLM 评审，真开闸时会被 RuleMinAmount 斩掉。11-04 的留痕里模型自己写出了这件事
//     （九芝堂："一手843元低于单笔下限880.34元，200股又超单票上限…无法合规建仓"）。
//
// minFen<=0（调用方没给下限口径）时退化成只判上界，与修正前逐位一致——不越权凭空
// 造一条限制线，这与 perSlot() 对 CapFen 的处理同一哲学。
func affordableStage(price model.Fen, capFen, minFen model.Fen) (bool, string) {
	if capFen <= 0 {
		return false, reasonUnaffordable
	}
	lot := price.Mul(lotsPerShare)
	if lot <= 0 {
		return false, reasonUnaffordable // 价格非法时不放行，与无资金口径同处理
	}
	if lot > capFen {
		return false, reasonUnaffordable
	}
	if minFen <= 0 || minFen > capFen {
		return true, "" // 无下界，或配置自相矛盾（由调用方记 Note 并把下界置 0）
	}
	// 存在正整数 n 使 n×lot ∈ [minFen, capFen]：n ≥ ceil(minFen/lot) 且 n ≤ floor(capFen/lot)。
	// lot>0 且 lot≤capFen 已在上两步保证，故 floor(capFen/lot) ≥ 1，不必再钳下界。
	nMin := (minFen + lot - 1) / lot
	if nMin > capFen/lot {
		return false, reasonLotWindow
	}
	return true, ""
}

// liquidityStage 流动性筛选：流通市值与换手率双门槛。
func liquidityStage(s model.StockBasic, cfg FilterConfig) (bool, string) {
	if s.CircMvW <= 0 || s.CircMvW < cfg.MinCircMvW {
		return false, reasonSmallMV
	}
	if s.TurnoverRate < cfg.MinTurnoverRate {
		return false, reasonIlliquid
	}
	return true, ""
}

// valuationStage 估值筛选：价格下限（仙股剔除）、PE（亏损剔除）、PB。
// price 是当日未复权收盘（现算自 daily_bar，不再从估值截面抄一份）。
// 价格上限不在这里——"买不买得起"由可用资金筛按现金实算。
func valuationStage(s model.StockBasic, price model.Fen, cfg FilterConfig) (bool, string) {
	if price.Float() < cfg.PriceLow {
		return false, reasonPriceOut
	}
	if s.PETtm <= 0 || s.PETtm > cfg.PETtmMax {
		return false, reasonBadPE
	}
	if s.PB <= 0 || s.PB > cfg.PBMax {
		return false, reasonBadPB
	}
	return true, ""
}

// hasValuation 今日是否有该股的估值截面。
//
// 估值列与股票静态属性同存一行，ValDate 说明它来自哪一天：拿上周的 PE 参与今天的
// 筛选是错的，所以日期不符等同于没有，不另做"就近取一天"的兜底。
func hasValuation(s model.StockBasic, tradeDate string) bool {
	return s.ValDate == tradeDate
}

// listDaysBetween 计算上市自然日数（listDate/tradeDate 均为 YYYYMMDD；解析失败返回 -1）。
//
// 以前用 YYYYMMDD 整数相减再按 y*365+m*30+d 换算，遇到"日/月借位"会把月份
// 字段吃掉：上市 20260125、交易日 20260210 只差 16 天，却算成 85 天；跨年
// 借位更夸张（20251220→20260301 实际 71 天，算成 2781 天）。60 日新股门槛
// 因此会被绕过。时间差交给 time.Parse 处理，不再手写日历。
func listDaysBetween(listDate, tradeDate string) int {
	ld, err := time.Parse("20060102", listDate)
	if err != nil {
		return -1
	}
	td, err := time.Parse("20060102", tradeDate)
	if err != nil {
		return -1
	}
	if td.Before(ld) {
		return -1
	}
	return int(td.Sub(ld).Hours() / 24)
}
