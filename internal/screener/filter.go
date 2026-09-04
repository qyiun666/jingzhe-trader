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
	"jingzhe-trader/internal/market"

	"jingzhe-trader/internal/model"
)

// FilterConfig 漏斗参数（全部可配，来源 config screen.* 键）。
type FilterConfig struct {
	TopN             int     // 候选池容量
	MinCircMvW       float64 // 流通市值下限（万元）
	MinTurnoverRate  float64 // 换手率下限（%）
	PriceLow         float64 // 价格下限（元，剔除仙股）
	PETtmMax         float64 // PE_TTM 上限（亏损股剔除）
	PBMax            float64 // PB 上限
	MinListDays      int     // 上市天数下限（新股剔除）
	SectorTopK       int     // 板块强弱保留前 K 个行业
	MinSectorMembers int     // 行业成员数下限（不足则该行业不参与排名）
}

// 漏斗各级淘汰原因（统一文案，只出现在日志与告警正文）。
const (
	reasonST             = "ST/风险警示"
	reasonNewStock       = "上市不足期"
	reasonSuspended      = "当日停牌"
	reasonNoIndustry     = "无行业归属"
	reasonSectorOut      = "非强势板块"
	reasonNoValuation    = "无当日估值截面"
	reasonUnaffordable   = "一手超单笔预算"
	reasonSmallMV        = "流通市值过小"
	reasonIlliquid       = "换手率不足"
	reasonPriceOut       = "价格过低"
	reasonBadPE          = "亏损或PE超标"
	reasonBadPB          = "PB超标"
	reasonRankOut        = "排名不足TopN"
	reasonMarketRegime   = "大盘跌破MA20，当日关闭买入漏斗"
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

// affordableStage 可用资金筛：一手成本必须落在单笔预算内，否则这只票买不起。
// budgetFen<=0 表示没有可用资金口径 —— 不放行（宁缺勿滥），由调用方记降级说明原因。
func affordableStage(price model.Fen, budgetFen model.Fen) (bool, string) {
	if budgetFen <= 0 {
		return false, reasonUnaffordable
	}
	if price.Mul(lotsPerShare) > budgetFen {
		return false, reasonUnaffordable
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

// listDaysBetween 计算上市天数（listDate/tradeDate 均为 YYYYMMDD；解析失败返回 -1）。
func listDaysBetween(listDate, tradeDate string) int {
	if len(listDate) != 8 || len(tradeDate) != 8 {
		return -1
	}
	ld, td := 0, 0
	for i := 0; i < 8; i++ {
		if listDate[i] < '0' || listDate[i] > '9' || tradeDate[i] < '0' || tradeDate[i] > '9' {
			return -1
		}
	}
	for i := 0; i < 8; i++ {
		ld = ld*10 + int(listDate[i]-'0')
		td = td*10 + int(tradeDate[i]-'0')
	}
	if td < ld {
		return -1
	}
	return approxDaysDiff(td - ld) // 粗略自然日差（足够 60 日门槛判断）
}

// approxDaysDiff 将 YYYYMMDD 差值粗略换算为自然日数（±3 天内误差可接受）。
func approxDaysDiff(raw int) int {
	y := raw / 10000
	m := (raw / 100) % 100
	d := raw % 100
	return y*365 + m*30 + d
}
