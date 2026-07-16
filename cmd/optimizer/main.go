package main

// 参数网格搜索工具 (Parameter Optimizer)
//
// 遍历均线交叉策略的参数组合 (short_period × long_period × position_pct),
// 对每组参数运行完整回测, 计算绩效指标, 最后按多个维度排序输出 TOP N。
//
// 用法:
//   go run ./cmd/optimizer -config config/config.yaml -strategy ma_cross \
//       -start 20240101 -end 20260715 -capital 10000
//
// 说明:
//   - 回测引擎内部已计算好 Metrics (CalculateMetrics), 这里直接读取 result.Metrics
//   - ma_cross 策略 Init 用 v.(float64) 解析参数, 因此 short/long 必须传 float64
//   - 批量回测时设置 Silent=true 并把日志级别调到 warn, 避免单次回测日志刷屏
//   - 每次回测后调用 engine.Close() 释放数据库连接, 防止连接泄漏

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/pkg/logger"
)

// OptResult 单组参数的回测结果
type OptResult struct {
	ShortPeriod  int     // 短均线周期
	LongPeriod   int     // 长均线周期
	PositionPct  float64 // 单票仓位占比
	TotalReturn  float64 // 总收益率
	AnnualReturn float64 // 年化收益率
	Sharpe       float64 // 夏普比率
	MaxDrawdown  float64 // 最大回撤
	TradeCount   int     // 交易次数
	WinRate      float64 // 胜率
	Err          error   // 回测出错时记录原因 (结果无效)
}

func main() {
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	strategyName := flag.String("strategy", "ma_cross", "策略名称")
	startDate := flag.String("start", "20240101", "回测起始日期 YYYYMMDD")
	endDate := flag.String("end", "20260715", "回测结束日期 YYYYMMDD")
	capital := flag.Float64("capital", 10000, "初始资金")
	universeStr := flag.String("universe", "000725.SZ,002230.SZ,002415.SZ,002475.SZ,000001.SZ,600030.SH,000625.SZ,601012.SZ,601899.SH,601318.SH,000333.SZ,600036.SH,600276.SH", "股票池(逗号分隔)")
	topN := flag.Int("top", 10, "输出前N个最优结果")
	flag.Parse()

	// 1. 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Printf("加载配置失败: %v\n", err)
		os.Exit(1)
	}

	// 2. 初始化日志: 批量回测时抑制 info 级别日志, 仅保留 warn/error, 避免刷屏
	_ = logger.Init("warn", cfg.Log.Format, "stdout", "")

	// 3. 解析股票池
	universe := strings.Split(*universeStr, ",")
	for i := range universe {
		universe[i] = strings.TrimSpace(universe[i])
	}

	// 4. 定义参数搜索网格
	shortPeriods := []int{3, 5, 7, 10}
	longPeriods := []int{10, 15, 20, 25, 30}
	positionPcts := []float64{0.25, 0.30, 0.35, 0.40}

	// 统计有效组合数 (剔除 short >= long 的组合)
	total := 0
	for _, sp := range shortPeriods {
		for _, lp := range longPeriods {
			if sp >= lp {
				continue
			}
			total += len(positionPcts)
		}
	}

	fmt.Printf("========== 参数网格搜索 ==========\n")
	fmt.Printf("策略:     %s\n", *strategyName)
	fmt.Printf("区间:     %s ~ %s\n", *startDate, *endDate)
	fmt.Printf("资金:     %.0f\n", *capital)
	fmt.Printf("股票池:   %d 只\n", len(universe))
	fmt.Printf("网格:     short%v x long%v x pos%v\n", shortPeriods, longPeriods, positionPcts)
	fmt.Printf("有效组合: %d (已剔除 short>=long)\n", total)
	fmt.Printf("==================================\n\n")

	// 5. 遍历所有参数组合运行回测
	var results []OptResult
	idx := 0
	start := time.Now()

	for _, sp := range shortPeriods {
		for _, lp := range longPeriods {
			// 短均线必须小于长均线, 否则无意义
			if sp >= lp {
				continue
			}
			for _, pp := range positionPcts {
				idx++
				fmt.Printf("\r[%d/%d] 测试: short=%-2d long=%-2d pos=%.0f%% ...", idx, total, sp, lp, pp*100)

				// 构建回测配置
				// 注意: ma_cross 策略 Init 用 v.(float64) 解析参数, 必须传 float64 类型
				btCfg := backtest.EngineConfig{
					StartDate:      *startDate,
					EndDate:        *endDate,
					InitialCapital: *capital,
					Universe:       universe,
					Benchmark:      cfg.Backtest.Benchmark,
					Slippage:       cfg.Backtest.Slippage,
					FillPrice:      cfg.Backtest.FillPrice,
					StrategyName:   *strategyName,
					StrategyParams: map[string]interface{}{
						"short_period": float64(sp),
						"long_period":  float64(lp),
						"position_pct": pp,
					},
					Silent: true, // 静默模式: 不打印单次回测摘要
				}

				engine, err := backtest.NewEngine(btCfg, cfg)
				if err != nil {
					results = append(results, OptResult{
						ShortPeriod: sp, LongPeriod: lp, PositionPct: pp, Err: err,
					})
					continue
				}

				result, err := engine.Run()
				_ = engine.Close() // 释放数据库连接, 避免批量回测时连接泄漏
				if err != nil {
					results = append(results, OptResult{
						ShortPeriod: sp, LongPeriod: lp, PositionPct: pp, Err: err,
					})
					continue
				}

				// 回测引擎内部已计算好指标, 直接读取
				m := result.Metrics
				results = append(results, OptResult{
					ShortPeriod:  sp,
					LongPeriod:   lp,
					PositionPct:  pp,
					TotalReturn:  m.TotalReturn,
					AnnualReturn: m.AnnualReturn,
					Sharpe:       m.SharpeRatio,
					MaxDrawdown:  m.MaxDrawdown,
					TradeCount:   m.TotalTrades,
					WinRate:      m.WinRate,
				})
			}
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("\r完成: %d 组合, 耗时 %s                      \n\n", idx, elapsed.Truncate(time.Second))

	// 6. 过滤掉出错的组合
	valid := make([]OptResult, 0, len(results))
	errCount := 0
	for _, r := range results {
		if r.Err != nil {
			errCount++
			continue
		}
		valid = append(valid, r)
	}
	if errCount > 0 {
		fmt.Printf("跳过 %d 个出错组合\n\n", errCount)
	}
	if len(valid) == 0 {
		fmt.Println("没有有效的回测结果, 请检查配置 / 数据 / 区间是否正确")
		os.Exit(1)
	}

	// 7. 综合排名 (夏普 + 总收益 + 年化 三个维度名次之和, 越小越好)
	printCompositeRank(valid, *topN)

	// 8. 各维度排序输出 TOP N
	printTopN("按夏普比率排序 (越高越好)", valid, *topN,
		func(a, b OptResult) bool { return a.Sharpe > b.Sharpe })
	printTopN("按总收益率排序 (越高越好)", valid, *topN,
		func(a, b OptResult) bool { return a.TotalReturn > b.TotalReturn })
	printTopN("按年化收益率排序 (越高越好)", valid, *topN,
		func(a, b OptResult) bool { return a.AnnualReturn > b.AnnualReturn })
	printTopN("按最大回撤排序 (越小越好)", valid, *topN,
		func(a, b OptResult) bool { return a.MaxDrawdown < b.MaxDrawdown })
}

// printTopN 按指定维度排序后输出 TOP N
// less: 返回 true 表示 a 应排在 b 前面 (更优)
func printTopN(title string, src []OptResult, topN int, less func(a, b OptResult) bool) {
	// 复制一份再排序, 不影响原切片
	data := make([]OptResult, len(src))
	copy(data, src)
	sort.Slice(data, func(i, j int) bool { return less(data[i], data[j]) })

	n := topN
	if n > len(data) {
		n = len(data)
	}

	fmt.Printf("========== %s TOP %d ==========\n", title, n)
	fmt.Printf("%-5s %-5s %-7s %-10s %-10s %-9s %-10s %-7s %-7s\n",
		"短", "长", "仓位", "总收益", "年化", "夏普", "回撤", "交易", "胜率")
	for i := 0; i < n; i++ {
		r := data[i]
		fmt.Printf("%-5d %-5d %-7.0f%% %-10.2f%% %-10.2f%% %-9.2f %-10.2f%% %-7d %-7.1f%%\n",
			r.ShortPeriod, r.LongPeriod, r.PositionPct*100,
			r.TotalReturn*100, r.AnnualReturn*100, r.Sharpe, r.MaxDrawdown*100,
			r.TradeCount, r.WinRate*100)
	}
	fmt.Println()
}

// printCompositeRank 输出综合排名
// 综合得分 = 夏普名次 + 总收益名次 + 年化名次 (名次越小越好, 总和越小综合表现越优)
func printCompositeRank(src []OptResult, topN int) {
	type ranked struct {
		r    OptResult
		rank int
	}
	data := make([]ranked, len(src))
	for i := range src {
		data[i] = ranked{r: src[i]}
	}

	// 累加各维度名次
	accumulate := func(less func(a, b OptResult) bool) {
		sort.Slice(data, func(i, j int) bool { return less(data[i].r, data[j].r) })
		for i := range data {
			data[i].rank += i // 第 i 名加 i 分 (0 最佳)
		}
	}
	accumulate(func(a, b OptResult) bool { return a.Sharpe > b.Sharpe })
	accumulate(func(a, b OptResult) bool { return a.TotalReturn > b.TotalReturn })
	accumulate(func(a, b OptResult) bool { return a.AnnualReturn > b.AnnualReturn })

	// 按综合名次升序 (越小越优)
	sort.Slice(data, func(i, j int) bool { return data[i].rank < data[j].rank })

	n := topN
	if n > len(data) {
		n = len(data)
	}

	fmt.Printf("========== 综合排名 TOP %d (夏普+总收益+年化 名次之和) ==========\n", n)
	fmt.Printf("%-4s %-5s %-5s %-7s %-10s %-10s %-9s %-10s %-7s %-7s\n",
		"名次", "短", "长", "仓位", "总收益", "年化", "夏普", "回撤", "交易", "胜率")
	for i := 0; i < n; i++ {
		r := data[i].r
		fmt.Printf("%-4d %-5d %-5d %-7.0f%% %-10.2f%% %-10.2f%% %-9.2f %-10.2f%% %-7d %-7.1f%%\n",
			i+1, r.ShortPeriod, r.LongPeriod, r.PositionPct*100,
			r.TotalReturn*100, r.AnnualReturn*100, r.Sharpe, r.MaxDrawdown*100,
			r.TradeCount, r.WinRate*100)
	}
	fmt.Println()
}
