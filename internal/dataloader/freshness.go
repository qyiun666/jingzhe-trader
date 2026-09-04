// Package dataloader 数据接入编排（L3）：日历/日线/财务同步 + 校验 + 新鲜度门禁。
//
// 依赖方向（ARCHITECTURE §1）：dataloader 依赖 store（仓储）、tushare/quote（适配层）、
// model、market、observability；不直接触网（网络只在适配层）。
package dataloader

import (
	"context"
	"fmt"
	"strings"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/store"
)

// 新鲜度门禁失败码（每项独立分支，均有单测，验收 #9）。
const (
	CodeCalMissing   = "CAL_MISSING"    // 交易日历缺失目标日
	CodeNotTradeDay  = "NOT_TRADE_DAY"  // 非交易日（非阻断，跳过）
	CodeBarStale     = "BAR_STALE"      // 目标日日线缺失（过时）
	CodeBarRowsLow   = "BAR_ROWS_LOW"   // 日线行数低于阈值
	CodeBasicRowsLow = "BASIC_ROWS_LOW" // 每日指标行数低于阈值
	CodeCoverageGap  = "COVERAGE_GAP"   // 候选/持仓覆盖缺口
	CodeIndexStale   = "INDEX_STALE"    // 沪深300指数日线缺失（阻断：买入闸门与大盘卖出规则都读它）
	CodeWindowShort  = "WINDOW_SHORT"   // 因子窗口内有交易日无日线（动量/MA20 会算错）
)

// freshnessIndex 新鲜度门禁锚定的大盘指数：与买入闸门用的是同一根常量。
const freshnessIndex = store.MarketIndex

// FreshnessGate 数据新鲜度门禁（八检查，每项独立失败码）。
//
// 检查项（ARCHITECTURE §7 / 任务规格）：
//  1. CalendarOK  交易日历含目标日（缺失→CAL_MISSING，阻断）
//  2. IsTradeDay  目标日为交易日（非交易日→NOT_TRADE_DAY，非阻断，跳过）
//  3. BarDate     目标日日线存在（缺失→BAR_STALE，阻断）
//  4. BarRows     日线行数≥阈值（不足→BAR_ROWS_LOW，阻断）
//  5. BasicRows   当日估值截面行数≥阈值（不足→BASIC_ROWS_LOW，阻断）
//  6. MissingCodes 候选∪持仓覆盖完整（缺口→COVERAGE_GAP，阻断）
//  7. WindowOK    因子窗口内每个交易日都有日线（缺口→WINDOW_SHORT，阻断）
//  8. IndexRows   大盘指数日线存在（缺失→INDEX_STALE，阻断：没有它买入闸门与大盘卖出规则都跑不了）
//
// Fresh = IsTradeDay && #1 && #3–#8（只有 #2 非阻断：非交易日直接跳过当日）。
type FreshnessGate struct {
	store      *store.Store
	minBarRows int // 日线/每日指标最低行数（来自 config screen.min_bar_rows）
	windowDays int // 因子窗口交易日数（选股是最深消费者）
}

// NewFreshnessGate 构造门禁。阈值由组合根从 config screen.min_bar_rows 给出（默认值只有
// KeySpec 一份），这里不再自带第二份默认；windowDays<=0 表示跳过窗口完整性检查。
func NewFreshnessGate(s *store.Store, minBarRows, windowDays int) *FreshnessGate {
	return &FreshnessGate{store: s, minBarRows: minBarRows, windowDays: windowDays}
}

// CheckItem 单项检查结果。
type CheckItem struct {
	Name     string // 检查名（如 CalendarOK）
	Code     string // 失败码；通过或跳过时为空
	OK       bool   // 是否通过
	Blocking bool   // 失败时是否阻断新鲜度（只有 IsTradeDay 非阻断：非交易日直接跳过当日）
	Detail   string
}

// FreshnessReport 门禁报告（可打印）。
type FreshnessReport struct {
	TradeDate string
	Fresh     bool // 数据是否新鲜可用
	Skipped   bool // 非交易日跳过
	Checks    []CheckItem
}

// String 返回可读报告。
func (r *FreshnessReport) String() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("数据新鲜度门禁报告（交易日 %s）\n", r.TradeDate))
	for _, c := range r.Checks {
		status := "PASS"
		switch {
		case !c.OK && c.Blocking:
			status = "FAIL"
		case !c.OK:
			status = "WARN"
		}
		b.WriteString(fmt.Sprintf("  [%s] %-12s %s\n", status, c.Name, c.Detail))
	}
	switch {
	case r.Skipped:
		b.WriteString("总体: 跳过（非交易日）\n")
	case r.Fresh:
		b.WriteString("总体: 新鲜\n")
	default:
		b.WriteString("总体: 不新鲜\n")
	}
	return b.String()
}

// Check 执行八项检查；任一项阻断性失败则 Fresh=false。
// 非交易日（#2）直接跳过后续数据检查并返回 Skipped。
func (g *FreshnessGate) Check(ctx context.Context, tradeDate string) (*FreshnessReport, error) {
	rep := &FreshnessReport{TradeDate: tradeDate}

	// 1. CalendarOK
	cal, err := g.store.MarketRepo().LoadTradeCal(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取交易日历失败: %w", err)
	}
	rep.Checks = append(rep.Checks, g.checkCalendar(cal, tradeDate))

	// 2. IsTradeDay（非阻断）
	isTradeDay := true
	rep.Checks = append(rep.Checks, g.checkIsTradeDay(cal, tradeDate, &isTradeDay))
	if !isTradeDay {
		rep.Skipped = true
		rep.Fresh = false
		return rep, nil
	}

	// 3-7. 数据新鲜度检查（除 IndexRows 外均阻断）
	rep.Checks = append(rep.Checks, g.checkBarDate(ctx, tradeDate))
	rep.Checks = append(rep.Checks, g.checkRows(ctx, tradeDate, "daily_bar", "trade_date", "BarRows", CodeBarRowsLow))
	// 估值截面在 stock_basic 的 val_date 上（不再有 daily_basic 表）：日期不符的行算"没有今日截面"。
	rep.Checks = append(rep.Checks, g.checkRows(ctx, tradeDate, "stock_basic", "val_date", "BasicRows", CodeBasicRowsLow))
	rep.Checks = append(rep.Checks, g.checkCoverage(ctx, tradeDate))
	rep.Checks = append(rep.Checks, g.checkWindow(ctx, tradeDate))
	rep.Checks = append(rep.Checks, g.checkIndex(ctx, tradeDate))

	// Fresh = 所有阻断性检查通过
	fresh := true
	for _, c := range rep.Checks {
		if !c.OK && c.Blocking {
			fresh = false
		}
	}
	rep.Fresh = fresh
	return rep, nil
}

func okItem(name, detail string) CheckItem {
	return CheckItem{Name: name, OK: true, Blocking: true, Detail: detail}
}

func failItem(name, code, detail string) CheckItem {
	return CheckItem{Name: name, Code: code, OK: false, Blocking: true, Detail: detail}
}

func warnItem(name, code, detail string) CheckItem {
	return CheckItem{Name: name, Code: code, OK: false, Blocking: false, Detail: detail}
}

// checkCalendar 检查交易日历是否含目标日。
func (g *FreshnessGate) checkCalendar(cal map[string]bool, tradeDate string) CheckItem {
	if _, ok := cal[tradeDate]; !ok {
		return failItem("CalendarOK", CodeCalMissing, fmt.Sprintf("交易日历缺失 %s", tradeDate))
	}
	return okItem("CalendarOK", "日历含目标日")
}

// checkIsTradeDay 检查目标日是否为交易日；非交易日为非阻断跳过项。
func (g *FreshnessGate) checkIsTradeDay(cal map[string]bool, tradeDate string, isTradeDay *bool) CheckItem {
	open := market.IsTradeDay(cal, tradeDate)
	*isTradeDay = open
	if !open {
		return warnItem("IsTradeDay", CodeNotTradeDay, fmt.Sprintf("%s 非交易日，跳过新鲜度检查", tradeDate))
	}
	return CheckItem{Name: "IsTradeDay", OK: true, Blocking: false, Detail: "交易日"}
}

// checkBarDate 检查目标日是否有日线（缺失即视为过时）。
func (g *FreshnessGate) checkBarDate(ctx context.Context, tradeDate string) CheckItem {
	n, err := g.store.MarketRepo().CountBar(ctx, tradeDate)
	if err != nil {
		return failItem("BarDate", CodeBarStale, fmt.Sprintf("统计日线失败: %v", err))
	}
	if n == 0 {
		return failItem("BarDate", CodeBarStale, fmt.Sprintf("%s 无日线数据（过时）", tradeDate))
	}
	return okItem("BarDate", fmt.Sprintf("日线覆盖 %d 只", n))
}

// checkRows 通用行数检查：某表按 dateCol 落在 tradeDate 上的行数是否≥阈值。
//
// table 与 dateCol 都只接受本文件内的常量，不接外部输入。
func (g *FreshnessGate) checkRows(ctx context.Context, tradeDate, table, dateCol, name, code string) CheckItem {
	var n int
	q := "SELECT COUNT(*) FROM " + table + " WHERE " + dateCol + " = ?"
	if err := g.store.ReadDB().GetContext(ctx, &n, q, tradeDate); err != nil {
		return failItem(name, code, fmt.Sprintf("统计%s失败: %v", table, err))
	}
	if n < g.minBarRows {
		return failItem(name, code, fmt.Sprintf("%s=%d < 阈值 %d", table, n, g.minBarRows))
	}
	return okItem(name, fmt.Sprintf("%s=%d", table, n))
}

// checkCoverage 检查候选池（list_status='L'）在目标日的日线覆盖是否完整（防漏数据）。
//
// 关键修正（避免历史版本"每天 0 候选"陷阱）：
//   - 排除当日停牌股：停牌本就无日线，若计入缺口会让"任何有停牌的日子"被判不新鲜→门禁永久失败→永远不出信号；
//   - 对极小的残余缺口（新股/数据抖动）仅告警不阻断；仅当缺口显著（真·数据未同步）才阻断。
func (g *FreshnessGate) checkCoverage(ctx context.Context, tradeDate string) CheckItem {
	cand, err := g.store.MarketRepo().CandidateCodes(ctx)
	if err != nil {
		return failItem("MissingCodes", CodeCoverageGap, fmt.Sprintf("读取候选池失败: %v", err))
	}
	if len(cand) == 0 {
		// 候选池为空（尚未同步股票基础信息）：视为无覆盖需求，跳过
		return okItem("MissingCodes", "候选池为空，无覆盖需求")
	}
	// 排除当日停牌股
	susp, err := g.store.MarketRepo().SuspendedCodes(ctx, tradeDate)
	if err != nil {
		return failItem("MissingCodes", CodeCoverageGap, fmt.Sprintf("读取停牌列表失败: %v", err))
	}
	excluded := make(map[string]struct{}, len(susp))
	for _, s := range susp {
		excluded[s] = struct{}{}
	}
	expected := make([]string, 0, len(cand)-len(susp))
	for _, c := range cand {
		if _, ok := excluded[c]; !ok {
			expected = append(expected, c)
		}
	}

	covered, err := g.store.MarketRepo().BarCoverage(ctx, tradeDate, expected)
	if err != nil {
		return failItem("MissingCodes", CodeCoverageGap, fmt.Sprintf("统计覆盖失败: %v", err))
	}
	gap := len(expected) - covered
	if gap <= 0 {
		return okItem("MissingCodes", fmt.Sprintf("覆盖 %d/%d（已排除停牌 %d 只）", covered, len(expected), len(susp)))
	}
	// 极小缺口（噪声/新股/数据抖动）仅告警不阻断；显著缺口才视为数据缺失（真·未同步）。
	tol := coverageTolerance(len(cand))
	if gap <= tol {
		return warnItem("MissingCodes", CodeCoverageGap,
			fmt.Sprintf("覆盖 %d/%d，缺口 %d 只（≤容差 %d，仅告警）", covered, len(expected), gap, tol))
	}
	return failItem("MissingCodes", CodeCoverageGap,
		fmt.Sprintf("覆盖 %d/%d，缺口 %d 只（>容差 %d，数据缺失）", covered, len(expected), gap, tol))
}

// coverageTolerance 覆盖缺口容差：候选池的 1% 或至少 50 只，取较大者。
// 目的是放过停牌/新股/数据抖动造成的微小缺口，只对显著缺失（真·数据未同步）阻断。
func coverageTolerance(candidateCount int) int {
	t := candidateCount / 100
	if t < 50 {
		t = 50
	}
	return t
}

// checkWindow 因子窗口完整性：最近 windowDays 个交易日历日都要有日线。
//
// 必须有这一项：单日行数达标不代表窗口完整。缺几天时因子窗口会算错（动量取首末、
// MA20 取近 20 根），而"当日数据出全"的其它检查全都会绿。
func (g *FreshnessGate) checkWindow(ctx context.Context, tradeDate string) CheckItem {
	if g.windowDays <= 0 {
		return CheckItem{Name: "WindowOK", OK: true, Detail: "未配置窗口天数，跳过"}
	}
	dates, err := g.store.ScreenRepo().WindowDates(ctx, tradeDate, g.windowDays)
	if err != nil {
		return failItem("WindowOK", CodeWindowShort, fmt.Sprintf("读取窗口交易日失败: %v", err))
	}
	if len(dates) < g.windowDays {
		return failItem("WindowOK", CodeWindowShort,
			fmt.Sprintf("交易日历只有 %d 个交易日 < 窗口 %d", len(dates), g.windowDays))
	}
	gaps, err := g.store.ScreenRepo().WindowBarGaps(ctx, dates)
	if err != nil {
		return failItem("WindowOK", CodeWindowShort, fmt.Sprintf("统计窗口覆盖失败: %v", err))
	}
	if len(gaps) > 0 {
		return failItem("WindowOK", CodeWindowShort,
			fmt.Sprintf("窗口 %d 日中缺 %d 天日线（%s 起），请补跑 daily", g.windowDays, len(gaps), gaps[0]))
	}
	return okItem("WindowOK", fmt.Sprintf("因子窗口 %d 个交易日齐全", g.windowDays))
}

// checkIndex 检查大盘指数日线是否存在。缺失即阻断：买入闸门（跌破 MA20 关漏斗）
// 与卖出规则（大盘恶化）都以这根指数为输入，没有它当日既不能买也不能判"大盘正常"。
func (g *FreshnessGate) checkIndex(ctx context.Context, tradeDate string) CheckItem {
	n, err := g.store.MarketRepo().CountIndexBar(ctx, freshnessIndex, tradeDate)
	if err != nil {
		return failItem("IndexRows", CodeIndexStale, fmt.Sprintf("统计指数日线失败: %v", err))
	}
	if n == 0 {
		return failItem("IndexRows", CodeIndexStale,
			fmt.Sprintf("%s 指数日线缺失（当日不开买入漏斗）", freshnessIndex))
	}
	return okItem("IndexRows", fmt.Sprintf("%s 指数日线存在", freshnessIndex))
}
