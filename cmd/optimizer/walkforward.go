package main

// Walk-Forward 样本外验证 (Walk-Forward Analysis)
//
// 把回测区间按时间切成 K 段 (-folds, 默认 3 段), 每段内:
//   1. 前 2/3 时间作为训练窗, 做网格搜索选出样本内最优参数
//   2. 后 1/3 时间作为测试窗, 用该参数跑样本外回测
// 最后拼接各段测试窗绩效, 输出 "样本内最优 vs 样本外实际" 对比表,
// 并评估参数稳定性 (相邻段最优参数是否漂移) 与样本外衰减率。
//
// 本文件的纯逻辑 (切窗 / 衰减率 / 参数漂移 / 加权推荐) 不依赖数据库, 可独立单测。

import (
	"fmt"
	"math"
	"sort"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/engine"
	"jingzhe-trader/internal/store"
)

// runSingleBacktest 运行单组参数的回测, 返回统一结果 (出错时 Err 非空, 结果无效)。
// 注意: ma_cross 策略 Init 用 v.(float64) 解析参数, short/long 必须传 float64 类型。
// dbPath 由 main 解析后显式传入 (而非取自配置): 每组参数各开一条连接跑完即关,
// 因为单条连接被 SetMaxOpenConns(1) 限死, 共享给 worker 池会让并行回测退化成串行。
func runSingleBacktest(dbPath string, cfg *config.Config, strategyName, startDate, endDate string,
	capital float64, universe []string, sp, lp int, pp float64) OptResult {

	btCfg := engine.RunConfig{
		StartDate:      startDate,
		EndDate:        endDate,
		InitialCapital: capital,
		Universe:       universe,
		Benchmark:      cfg.Backtest.Benchmark,
		Slippage:       cfg.Backtest.Slippage,
		FillPrice:      cfg.Backtest.FillPrice,
		StrategyName:   strategyName,
		StrategyParams: map[string]interface{}{
			"short_period": float64(sp),
			"long_period":  float64(lp),
			"position_pct": pp,
		},
		Silent: true, // 静默模式: 不打印单次回测摘要
	}

	fail := func(err error) OptResult {
		return OptResult{ShortPeriod: sp, LongPeriod: lp, PositionPct: pp, Err: err}
	}
	db, err := store.NewDB(dbPath)
	if err != nil {
		return fail(fmt.Errorf("打开数据库失败: %w", err))
	}
	defer db.Close()

	runner, err := engine.NewBacktestRunner(db, btCfg, cfg)
	if err != nil {
		return fail(err)
	}
	result, err := runner.Run()
	if err != nil {
		return fail(err)
	}

	// 回测引擎内部已计算好指标, 直接读取
	m := result.Metrics
	return OptResult{
		ShortPeriod:  sp,
		LongPeriod:   lp,
		PositionPct:  pp,
		TotalReturn:  m.TotalReturn,
		AnnualReturn: m.AnnualReturn,
		Sharpe:       m.SharpeRatio,
		MaxDrawdown:  m.MaxDrawdown,
		TradeCount:   m.TotalTrades,
		WinRate:      m.WinRate,
	}
}

// paramGrid 参数搜索网格
type paramGrid struct {
	ShortPeriods []int     // 短均线周期候选
	LongPeriods  []int     // 长均线周期候选
	PositionPcts []float64 // 单票仓位占比候选
}

// combos 展开有效参数组合 (剔除 short >= long 的无意义组合)
func (g paramGrid) combos() [][3]interface{} {
	var out [][3]interface{}
	for _, sp := range g.ShortPeriods {
		for _, lp := range g.LongPeriods {
			if sp >= lp {
				continue
			}
			for _, pp := range g.PositionPcts {
				out = append(out, [3]interface{}{sp, lp, pp})
			}
		}
	}
	return out
}

// paramKey 一组参数的标识 (用于加权投票时聚合)
type paramKey struct {
	short int
	long  int
	pos   float64
}

func keyOf(r OptResult) paramKey {
	return paramKey{short: r.ShortPeriod, long: r.LongPeriod, pos: r.PositionPct}
}

// WFWindow 一段 walk-forward 窗口: 训练窗 + 测试窗
type WFWindow struct {
	Index      int    // 段序号 (从 1 开始)
	TrainStart string // 训练窗起始 YYYYMMDD
	TrainEnd   string // 训练窗结束 YYYYMMDD
	TestStart  string // 测试窗起始 YYYYMMDD
	TestEnd    string // 测试窗结束 YYYYMMDD
}

const dateLayout = "20060102"

// splitWalkForwardFolds 把 [start, end] 按日历时间均分为 folds 段,
// 每段内前 2/3 时间为训练窗, 后 1/3 为测试窗。
// 分段按天数切 (非交易日), 回测引擎内部会按交易日历对齐;
// 整除余数归入最后一段。
func splitWalkForwardFolds(start, end string, folds int) ([]WFWindow, error) {
	startT, err := time.Parse(dateLayout, start)
	if err != nil {
		return nil, fmt.Errorf("起始日期格式错误 (%s): %w", start, err)
	}
	endT, err := time.Parse(dateLayout, end)
	if err != nil {
		return nil, fmt.Errorf("结束日期格式错误 (%s): %w", end, err)
	}
	if !endT.After(startT) {
		return nil, fmt.Errorf("结束日期必须晚于起始日期: %s ~ %s", start, end)
	}
	if folds < 1 {
		return nil, fmt.Errorf("段数必须 >= 1, 当前: %d", folds)
	}

	totalDays := int(endT.Sub(startT).Hours()/24) + 1
	segLen := totalDays / folds
	// 每段至少要能切出 1 天训练 + 1 天测试, 留足余量要求 >= 3 天
	if segLen < 3 {
		return nil, fmt.Errorf("区间共 %d 天, 切 %d 段后每段不足 3 天, 请减少 -folds", totalDays, folds)
	}

	windows := make([]WFWindow, 0, folds)
	segStart := startT
	for i := 0; i < folds; i++ {
		segEnd := segStart.AddDate(0, 0, segLen-1)
		if i == folds-1 {
			segEnd = endT // 最后一段吃掉整除余数
		}
		segDays := int(segEnd.Sub(segStart).Hours()/24) + 1
		trainDays := segDays * 2 / 3
		if trainDays < 1 {
			trainDays = 1
		}
		if trainDays >= segDays {
			trainDays = segDays - 1 // 至少留 1 天给测试窗
		}
		windows = append(windows, WFWindow{
			Index:      i + 1,
			TrainStart: segStart.Format(dateLayout),
			TrainEnd:   segStart.AddDate(0, 0, trainDays-1).Format(dateLayout),
			TestStart:  segStart.AddDate(0, 0, trainDays).Format(dateLayout),
			TestEnd:    segEnd.Format(dateLayout),
		})
		segStart = segEnd.AddDate(0, 0, 1)
	}
	return windows, nil
}

// decayRate 样本外衰减率: (样本内 - 样本外) / |样本内|
// 返回值 > 0 表示样本外变差 (过拟合衰减), < 0 表示样本外反而更好;
// 样本内指标为 0 时衰减率无意义, 返回 ok=false。
func decayRate(inSample, outSample float64) (rate float64, ok bool) {
	if inSample == 0 {
		return 0, false
	}
	return (inSample - outSample) / math.Abs(inSample), true
}

// paramDrift 相邻两段最优参数的漂移情况:
// 返回变化的维度数 (0~3) 及漂移明细文案。
func paramDrift(a, b paramKey) (changed int, detail string) {
	if a.short != b.short {
		changed++
		detail += fmt.Sprintf("短均线 %d->%d ", a.short, b.short)
	}
	if a.long != b.long {
		changed++
		detail += fmt.Sprintf("长均线 %d->%d ", a.long, b.long)
	}
	if a.pos != b.pos {
		changed++
		detail += fmt.Sprintf("仓位 %.0f%%->%.0f%%", a.pos*100, b.pos*100)
	}
	if changed == 0 {
		detail = "无漂移"
	}
	return changed, detail
}

// bestByComposite 按综合排名 (夏普+总收益+年化 名次之和) 选出最优参数,
// 与主流程 printCompositeRank 同一口径。出错组合已被调用方过滤。
func bestByComposite(results []OptResult) (OptResult, bool) {
	if len(results) == 0 {
		return OptResult{}, false
	}
	idx := make([]int, len(results))
	for i := range idx {
		idx[i] = i
	}
	rank := make([]int, len(results))
	accumulate := func(less func(a, b OptResult) bool) {
		sort.Slice(idx, func(i, j int) bool { return less(results[idx[i]], results[idx[j]]) })
		for pos, ri := range idx {
			rank[ri] += pos // 第 pos 名加 pos 分 (0 最佳)
		}
	}
	accumulate(func(a, b OptResult) bool { return a.Sharpe > b.Sharpe })
	accumulate(func(a, b OptResult) bool { return a.TotalReturn > b.TotalReturn })
	accumulate(func(a, b OptResult) bool { return a.AnnualReturn > b.AnnualReturn })

	best := 0
	for i := range rank {
		if rank[i] < rank[best] {
			best = i
		}
	}
	return results[best], true
}

// foldResult 一段 walk-forward 的评估结果
type foldResult struct {
	Window WFWindow  // 切窗信息
	Best   OptResult // 训练窗样本内最优
	OOS    OptResult // 该参数在测试窗的样本外表现
}

// oosWeight 样本外权重: 夏普与总收益的正部之和。
// 样本外表现为负的参数权重为 0, 不参与最终推荐。
func oosWeight(r OptResult) float64 {
	return math.Max(r.Sharpe, 0) + math.Max(r.TotalReturn, 0)
}

// recommendParam 按样本外夏普/收益加权投票, 给出最终推荐参数:
// 每段训练窗最优参数按其在测试窗的样本外表现加权, 相同参数权重累加, 取总权重最高者。
// 若所有段样本外权重均为 0 (全盘亏损), 退化为取样本外 (夏普+总收益) 最高的一段。
// 返回推荐参数与样本外综合得分, ok=false 表示没有任何可用段。
func recommendParam(folds []foldResult) (paramKey, float64, bool) {
	if len(folds) == 0 {
		return paramKey{}, 0, false
	}
	type agg struct {
		weight float64
		score  float64 // 样本外 夏普+总收益, 用于权重全 0 时的兜底比较
	}
	byParam := make(map[paramKey]*agg)
	var order []paramKey // 保持出现顺序, 保证同分时结果稳定
	for _, f := range folds {
		k := keyOf(f.OOS)
		s := f.OOS.Sharpe + f.OOS.TotalReturn
		a, ok := byParam[k]
		if !ok {
			a = &agg{score: s} // 首个样本直接记录得分 (可为负, 供全亏兜底比较)
			byParam[k] = a
			order = append(order, k)
		} else if s > a.score {
			a.score = s
		}
		a.weight += oosWeight(f.OOS)
	}
	var best paramKey
	bestSet := false
	bestWeight, bestScore := 0.0, 0.0
	for _, k := range order {
		a := byParam[k]
		if !bestSet || a.weight > bestWeight || (a.weight == bestWeight && a.score > bestScore) {
			best, bestWeight, bestScore = k, a.weight, a.score
			bestSet = true
		}
	}
	return best, bestScore, bestSet
}

// stitchedOOS 拼接各段测试窗绩效后的汇总指标
type stitchedOOS struct {
	TotalReturn float64 // 各段测试窗总收益按复利连乘
	Sharpe      float64 // 各段夏普按测试窗天数加权平均
	MaxDrawdown float64 // 各段最大回撤的最大值 (近似)
}

// stitchOOS 拼接各段测试窗绩效:
// 总收益按复利连乘 (1+r1)(1+r2)...-1, 夏普按测试窗天数加权, 回撤取各段最大值。
func stitchOOS(folds []foldResult) stitchedOOS {
	var out stitchedOOS
	prod := 1.0
	sharpeSum, daySum := 0.0, 0.0
	for _, f := range folds {
		prod *= 1 + f.OOS.TotalReturn
		if f.OOS.MaxDrawdown > out.MaxDrawdown {
			out.MaxDrawdown = f.OOS.MaxDrawdown
		}
		// 测试窗天数作为夏普权重
		s, err1 := time.Parse(dateLayout, f.Window.TestStart)
		e, err2 := time.Parse(dateLayout, f.Window.TestEnd)
		days := 1.0
		if err1 == nil && err2 == nil {
			days = e.Sub(s).Hours()/24 + 1
		}
		sharpeSum += f.OOS.Sharpe * days
		daySum += days
	}
	out.TotalReturn = prod - 1
	if daySum > 0 {
		out.Sharpe = sharpeSum / daySum
	}
	return out
}

// runWalkForward walk-forward 模式主流程
func runWalkForward(dbPath string, cfg *config.Config, strategyName, startDate, endDate string,
	capital float64, universe []string, grid paramGrid, folds int, parallelWorkers int) {

	windows, err := splitWalkForwardFolds(startDate, endDate, folds)
	if err != nil {
		fmt.Printf("切窗失败: %v\n", err)
		return
	}
	combos := grid.combos()

	fmt.Printf("========== Walk-Forward 样本外验证 ==========\n")
	fmt.Printf("策略:     %s\n", strategyName)
	fmt.Printf("区间:     %s ~ %s\n", startDate, endDate)
	fmt.Printf("资金:     %.0f\n", capital)
	fmt.Printf("股票池:   %d 只\n", len(universe))
	fmt.Printf("分段:     %d 段 (每段前 2/3 训练, 后 1/3 样本外测试)\n", len(windows))
	fmt.Printf("每段组合: %d (已剔除 short>=long)\n", len(combos))
	fmt.Printf("=============================================\n\n")

	start := time.Now()
	var foldResults []foldResult

	for _, w := range windows {
		fmt.Printf("----- 段 %d/%d: 训练 %s~%s  测试 %s~%s -----\n",
			w.Index, len(windows), w.TrainStart, w.TrainEnd, w.TestStart, w.TestEnd)

		// 1. 训练窗网格搜索 (并行, 结果按组合顺序收集)
		trainResults := runParallelBacktests(dbPath, cfg, strategyName, w.TrainStart, w.TrainEnd,
			capital, universe, combos, parallelWorkers)
		var valid []OptResult
		for _, r := range trainResults {
			if r.Err == nil {
				valid = append(valid, r)
			}
		}
		fmt.Println()
		if len(valid) == 0 {
			fmt.Printf("  段 %d 训练窗无有效结果, 跳过\n\n", w.Index)
			continue
		}

		// 2. 选样本内最优, 跑测试窗样本外回测
		best, _ := bestByComposite(valid)
		oos := runSingleBacktest(dbPath, cfg, strategyName, w.TestStart, w.TestEnd, capital, universe,
			best.ShortPeriod, best.LongPeriod, best.PositionPct)
		if oos.Err != nil {
			fmt.Printf("  段 %d 测试窗回测失败: %v, 跳过\n\n", w.Index, oos.Err)
			continue
		}
		fmt.Printf("  样本内最优: short=%d long=%d pos=%.0f%% | IS 收益 %.2f%% 夏普 %.2f | OOS 收益 %.2f%% 夏普 %.2f\n\n",
			best.ShortPeriod, best.LongPeriod, best.PositionPct*100,
			best.TotalReturn*100, best.Sharpe, oos.TotalReturn*100, oos.Sharpe)

		foldResults = append(foldResults, foldResult{Window: w, Best: best, OOS: oos})
	}

	elapsed := time.Since(start)
	fmt.Printf("完成: %d/%d 段有效, 耗时 %s\n\n", len(foldResults), len(windows), elapsed.Truncate(time.Second))
	if len(foldResults) == 0 {
		fmt.Println("没有有效的 walk-forward 结果, 请检查配置 / 数据 / 区间是否正确")
		return
	}

	printWFCompareTable(foldResults)
	printWFStability(foldResults)
	printWFRecommendation(foldResults)
}

// printWFCompareTable 输出 "样本内最优 vs 样本外实际" 对比表及拼接绩效、衰减率
func printWFCompareTable(folds []foldResult) {
	fmt.Printf("========== 样本内最优 vs 样本外实际 对比 ==========\n")
	fmt.Printf("%-4s %-19s %-16s %-9s %-7s %-9s | %-9s %-7s %-9s | %-8s\n",
		"段", "训练窗", "最优参数(短/长/仓)", "IS收益", "IS夏普", "IS回撤",
		"OOS收益", "OOS夏普", "OOS回撤", "夏普衰减")
	for _, f := range folds {
		is, oos := f.Best, f.OOS
		decay := "N/A"
		if d, ok := decayRate(is.Sharpe, oos.Sharpe); ok {
			decay = fmt.Sprintf("%.1f%%", d*100)
		}
		fmt.Printf("%-4d %-19s %-16s %-9.2f%% %-7.2f %-9.2f%% | %-9.2f%% %-7.2f %-9.2f%% | %-8s\n",
			f.Window.Index,
			f.Window.TrainStart+"~"+f.Window.TrainEnd,
			fmt.Sprintf("%d/%d/%.0f%%", is.ShortPeriod, is.LongPeriod, is.PositionPct*100),
			is.TotalReturn*100, is.Sharpe, is.MaxDrawdown*100,
			oos.TotalReturn*100, oos.Sharpe, oos.MaxDrawdown*100,
			decay)
	}

	// 拼接各段测试窗绩效
	st := stitchOOS(folds)
	fmt.Printf("\n----- 拼接样本外绩效 (%d 段测试窗) -----\n", len(folds))
	fmt.Printf("总收益: %.2f%%  夏普(天数加权): %.2f  最大回撤: %.2f%%\n",
		st.TotalReturn*100, st.Sharpe, st.MaxDrawdown*100)

	// 整体样本外衰减率: 样本内夏普均值 vs 拼接样本外夏普
	isSharpeSum := 0.0
	for _, f := range folds {
		isSharpeSum += f.Best.Sharpe
	}
	isSharpeAvg := isSharpeSum / float64(len(folds))
	if d, ok := decayRate(isSharpeAvg, st.Sharpe); ok {
		fmt.Printf("样本外衰减率: %.1f%% (样本内夏普均值 %.2f -> 样本外夏普 %.2f)\n",
			d*100, isSharpeAvg, st.Sharpe)
	} else {
		fmt.Printf("样本外衰减率: N/A (样本内夏普均值为 0)\n")
	}
	fmt.Println()
}

// printWFStability 输出参数稳定性: 相邻段最优参数是否漂移
func printWFStability(folds []foldResult) {
	fmt.Printf("========== 参数稳定性 (相邻段漂移) ==========\n")
	if len(folds) < 2 {
		fmt.Printf("仅 %d 段有效, 无法评估相邻段漂移\n\n", len(folds))
		return
	}
	totalChanged := 0
	pairs := 0
	for i := 1; i < len(folds); i++ {
		prev, cur := keyOf(folds[i-1].Best), keyOf(folds[i].Best)
		changed, detail := paramDrift(prev, cur)
		totalChanged += changed
		pairs++
		fmt.Printf("段 %d -> 段 %d: %s\n", folds[i-1].Window.Index, folds[i].Window.Index, detail)
	}
	// 漂移率 = 变化维度数 / (相邻段对数 x 3 个维度)
	driftRatio := float64(totalChanged) / float64(pairs*3)
	fmt.Printf("漂移率: %.1f%% (%d 个相邻段对共变化 %d 个维度, 0%% 完全稳定)\n\n",
		driftRatio*100, pairs, totalChanged)
}

// printWFRecommendation 输出按样本外夏普/收益加权排名的最终推荐参数
func printWFRecommendation(folds []foldResult) {
	fmt.Printf("========== 最终推荐 (样本外夏普/收益加权) ==========\n")
	// 各段样本外权重明细
	for _, f := range folds {
		fmt.Printf("段 %d: 参数 %d/%d/%.0f%%  样本外权重 %.3f (夏普 %.2f + 收益 %.2f%%)\n",
			f.Window.Index, f.OOS.ShortPeriod, f.OOS.LongPeriod, f.OOS.PositionPct*100,
			oosWeight(f.OOS), f.OOS.Sharpe, f.OOS.TotalReturn*100)
	}
	best, score, ok := recommendParam(folds)
	if !ok {
		fmt.Println("无可用段, 无法给出推荐")
		return
	}
	fmt.Printf("\n推荐参数: short=%d long=%d pos=%.0f%% (样本外综合得分 %.3f)\n",
		best.short, best.long, best.pos*100, score)
	fmt.Println("提示: 推荐基于样本外表现加权投票, 请结合上方衰减率与漂移率判断过拟合风险")
}
