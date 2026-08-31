package main

// 并行回测: 参数组合间完全独立 (每个 runSingleBacktest 自开数据库连接),
// 用固定 worker 池并行运行, 结果按下标顺序收集, 与串行输出完全一致。
// 单组合失败不中断整体 (沿用 OptResult.Err 字段标记), 与串行行为一致。

import (
	"fmt"
	"sync"

	"jingzhe-trader/internal/config"
)

// runParallel 并行执行任务并按下标顺序收集结果
// run 必须线程安全; workers <= 0 时退化为串行
func runParallel(combos [][3]interface{}, workers int, run func(c [3]interface{}) OptResult) []OptResult {
	results := make([]OptResult, len(combos))
	if len(combos) == 0 {
		return results
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(combos) {
		workers = len(combos)
	}

	type job struct {
		idx   int
		combo [3]interface{}
	}
	jobs := make(chan job, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	done := 0
	total := len(combos)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results[j.idx] = run(j.combo)
				mu.Lock()
				done++
				fmt.Printf("\r[%d/%d] 完成...", done, total)
				mu.Unlock()
			}
		}()
	}
	for i, c := range combos {
		jobs <- job{idx: i, combo: c}
	}
	close(jobs)
	wg.Wait()
	return results
}

// runParallelBacktests 并行运行多组参数回测 (包装 runSingleBacktest)
func runParallelBacktests(dbPath string, cfg *config.Config, strategyName, startDate, endDate string,
	capital float64, universe []string, combos [][3]interface{}, workers int) []OptResult {

	return runParallel(combos, workers, func(c [3]interface{}) OptResult {
		sp, lp, pp := c[0].(int), c[1].(int), c[2].(float64)
		return runSingleBacktest(dbPath, cfg, strategyName, startDate, endDate, capital, universe, sp, lp, pp)
	})
}
