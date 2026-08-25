package main

// runParallel 并行执行核心逻辑测试 (注入假 run, 不依赖数据库与回测引擎)
// 守护: 结果顺序与串行一致 / 错误传播 / worker 数变化结果稳定 / 空输入

import (
	"fmt"
	"testing"
)

// fakeRun 生成可控结果: 结果携带组合参数, long==15 时模拟回测失败
func fakeRun(c [3]interface{}) OptResult {
	sp, lp := c[0].(int), c[1].(int)
	pp := c[2].(float64)
	if lp == 15 {
		return OptResult{ShortPeriod: sp, LongPeriod: lp, PositionPct: pp, Err: fmt.Errorf("模拟回测失败")}
	}
	return OptResult{ShortPeriod: sp, LongPeriod: lp, PositionPct: pp, Sharpe: float64(sp) * 10}
}

// makeCombos 构造 n 组可预测组合 (short=3.., long=10.., pos=0.25..)
func makeCombos(n int) [][3]interface{} {
	combos := make([][3]interface{}, 0, n)
	for i := 0; i < n; i++ {
		combos = append(combos, [3]interface{}{3 + i, 10 + i, 0.25 + 0.01*float64(i)})
	}
	return combos
}

// verifyOrder 校验结果顺序与输入顺序一致 (组合参数回填到结果)
func verifyOrder(t *testing.T, combos [][3]interface{}, results []OptResult) {
	t.Helper()
	if len(results) != len(combos) {
		t.Fatalf("结果数 = %d, 期望 %d", len(results), len(combos))
	}
	for i, c := range combos {
		r := results[i]
		if r.ShortPeriod != c[0].(int) || r.LongPeriod != c[1].(int) || r.PositionPct != c[2].(float64) {
			t.Errorf("结果[%d] = %d/%d/%.2f, 期望 %d/%d/%.2f (顺序错乱)",
				i, r.ShortPeriod, r.LongPeriod, r.PositionPct, c[0].(int), c[1].(int), c[2].(float64))
		}
	}
}

func TestRunParallelOrder(t *testing.T) {
	combos := makeCombos(20)
	results := runParallel(combos, 4, fakeRun)
	verifyOrder(t, combos, results)
}

func TestRunParallelErrorPropagation(t *testing.T) {
	combos := makeCombos(20)
	results := runParallel(combos, 3, fakeRun)
	errCount := 0
	for i, r := range results {
		if r.LongPeriod == 15 {
			if r.Err == nil {
				t.Errorf("结果[%d] (long=15) 期望 Err 但为 nil", i)
			}
			errCount++
		} else if r.Err != nil {
			t.Errorf("结果[%d] 不应有 Err: %v", i, r.Err)
		}
	}
	if errCount == 0 {
		t.Fatal("期望至少一个模拟失败组合, 测试数据构造有误")
	}
}

func TestRunParallelWorkersStable(t *testing.T) {
	combos := makeCombos(20)
	// 串行 (workers=1) 与并行 (workers=7) 结果必须完全一致
	serial := runParallel(combos, 1, fakeRun)
	parallel := runParallel(combos, 7, fakeRun)
	for i := range serial {
		if serial[i].Err != nil || parallel[i].Err != nil {
			if (serial[i].Err == nil) != (parallel[i].Err == nil) {
				t.Fatalf("结果[%d] Err 状态不一致: serial=%v parallel=%v", i, serial[i].Err, parallel[i].Err)
			}
			continue
		}
		if serial[i].Sharpe != parallel[i].Sharpe {
			t.Errorf("结果[%d] Sharpe 不一致: serial=%.2f parallel=%.2f", i, serial[i].Sharpe, parallel[i].Sharpe)
		}
	}
	verifyOrder(t, combos, serial)
	verifyOrder(t, combos, parallel)
}

func TestRunParallelZeroWorkersSerialFallback(t *testing.T) {
	// workers=0 退化为串行, 结果与 workers=1 一致
	combos := makeCombos(20)
	fallback := runParallel(combos, 0, fakeRun)
	serial := runParallel(combos, 1, fakeRun)
	for i := range serial {
		if (fallback[i].Err == nil) != (serial[i].Err == nil) {
			t.Fatalf("结果[%d] Err 状态不一致: fallback=%v serial=%v", i, fallback[i].Err, serial[i].Err)
		}
		if fallback[i].Err == nil && fallback[i].Sharpe != serial[i].Sharpe {
			t.Errorf("结果[%d] Sharpe 不一致: fallback=%.2f serial=%.2f", i, fallback[i].Sharpe, serial[i].Sharpe)
		}
	}
}

func TestRunParallelEmpty(t *testing.T) {
	if results := runParallel(nil, 4, fakeRun); len(results) != 0 {
		t.Errorf("空输入结果数 = %d, 期望 0", len(results))
	}
}

func TestRunParallelWorkersCap(t *testing.T) {
	// worker 数大于组合数时不 panic 且结果完整
	combos := makeCombos(3)
	results := runParallel(combos, 16, fakeRun)
	verifyOrder(t, combos, results)
}

func TestRunParallelConcurrencySafety(t *testing.T) {
	// 高并发下共享槽位写入竞态检测 (配合 -race 运行)
	combos := makeCombos(200)
	results := runParallel(combos, 8, fakeRun)
	verifyOrder(t, combos, results)
}
