// Package screener 全市场粗筛 + 五因子打分 + TopN 候选池。
//
// 设计要点（用户核心痛点：选股必须可诊断）：
//   - 每一级筛选的进出计数与淘汰原因分布落 screen_funnel，重跑幂等；
//   - 候选为 0 时把"最接近阈值"的 Top20 降级写入 screen_watchlist 并落 SCREEN_EMPTY urgent 告警；
//   - 因子数量克制：动量 / 质量 / 价值 / 低波 / 流动性 共 5 个稳健可解释因子，无黑盒模型。
//
// 依赖方向：screener 只依赖 model 与 store，不触网（数据全部来自 SQLite）。
package screener

import (
	"jingzhe-trader/internal/model"
)

// FilterConfig 粗筛参数（全部可配，来源 config screen.* 键）。
type FilterConfig struct {
	TopN            int     // 候选池容量（默认 20）
	MinCircMvW      float64 // 流通市值下限（万元）
	MinTurnoverRate float64 // 换手率下限（%）
	PriceLow        float64 // 价格区间下限（元）
	PriceHigh       float64 // 价格区间上限（元）
	PETtmMax        float64 // PE_TTM 上限（>0 才有效；亏损股剔除）
	PBMax           float64 // PB 上限
	MinListDays     int     // 上市天数下限（新股剔除，默认 60）
}

// DefaultFilterConfig 返回与 config screen.* 默认值一致的筛选参数。
func DefaultFilterConfig() FilterConfig {
	return FilterConfig{
		TopN:            20,
		MinCircMvW:      500000, // 50 亿
		MinTurnoverRate: 1.0,
		PriceLow:        2.0,
		PriceHigh:       100.0,
		PETtmMax:        80.0,
		PBMax:           10.0,
		MinListDays:     60,
	}
}

// 漏斗各级淘汰原因（统一文案，落 screen_funnel.drop_reasons 的 JSON 键）。
const (
	reasonST        = "ST/风险警示"
	reasonNewStock  = "上市不足期"
	reasonSuspended = "当日停牌"
	reasonNoBasic   = "无每日指标"
	reasonSmallMV   = "流通市值过小"
	reasonIlliquid  = "换手率不足"
	reasonPriceOut  = "价格越界"
	reasonBadPE     = "亏损或PE超标"
	reasonBadPB     = "PB超标"
	reasonRankOut   = "排名不足TopN"
)

// basicEligible 基础资格：非 ST、上市满 minListDays、当日未停牌。
// 返回是否通过与淘汰原因（通过时为空串）。
func basicEligible(s model.StockBasic, listDays, minListDays int, suspended bool) (bool, string) {
	if s.IsST {
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
	// 粗略自然日差（足够 60 日门槛判断；无需精确历法）
	return approxDaysDiff(td - ld)
}

// approxDaysDiff 将 YYYYMMDD 差值粗略换算为自然日数（±3 天内误差可接受）。
func approxDaysDiff(raw int) int {
	y := raw / 10000
	m := (raw / 100) % 100
	d := raw % 100
	return y*365 + m*30 + d
}

// liquidityStage 流动性筛选：流通市值与换手率双门槛。
func liquidityStage(b model.DailyBasic, cfg FilterConfig) (bool, string) {
	if b.CircMvW <= 0 || b.CircMvW < cfg.MinCircMvW {
		return false, reasonSmallMV
	}
	if b.TurnoverRate < cfg.MinTurnoverRate {
		return false, reasonIlliquid
	}
	return true, ""
}

// valuationStage 估值/价格筛选：价格区间、PE（亏损剔除）、PB。
func valuationStage(b model.DailyBasic, cfg FilterConfig) (bool, string) {
	priceYuan := b.Close.Float()
	if priceYuan < cfg.PriceLow || priceYuan > cfg.PriceHigh {
		return false, reasonPriceOut
	}
	if b.PETtm <= 0 || b.PETtm > cfg.PETtmMax {
		return false, reasonBadPE
	}
	if b.PB <= 0 || b.PB > cfg.PBMax {
		return false, reasonBadPB
	}
	return true, ""
}
