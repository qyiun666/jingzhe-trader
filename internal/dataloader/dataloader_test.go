package dataloader

// syncByTradeDay 并发改造测试 (注入 fake repo/fetch/store, 不依赖数据库与网络)

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"jingzhe-trader/internal/model"
)

// fakeMaxDateRepo 实现 maxDateRepo 接口
type fakeMaxDateRepo struct {
	maxDate string
}

func (r *fakeMaxDateRepo) GetMaxTradeDate() (string, error) {
	return r.maxDate, nil
}

// fakeFetch 记录被调用的交易日并返回可预期数据 (并发安全: syncByTradeDay 并发拉取)
type fakeFetch struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeFetch) fn(calDate string) ([]int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, calDate)
	f.mu.Unlock()
	return []int{1, 2, 3}, nil
}

// tradeCals 构造 n 个连续交易日
func tradeCals(n int) []model.TradeCal {
	cals := make([]model.TradeCal, 0, n)
	for i := 0; i < n; i++ {
		cals = append(cals, model.TradeCal{CalDate: fmt.Sprintf("202601%02d", i+1)})
	}
	return cals
}

func TestSyncByTradeDaySkipsSyncedDates(t *testing.T) {
	repo := &fakeMaxDateRepo{maxDate: "20260105"} // 已同步到 01-05, 之后的才拉取
	fetch := &fakeFetch{}
	var storeCalls atomic.Int64

	cals := tradeCals(10)
	synced, attempts := syncByTradeDay(repo, cals, "测试", fetch.fn, func(items []int) error {
		storeCalls.Add(1)
		return nil
	})

	// 01-06 ~ 01-10 共 5 个交易日应被拉取
	if len(fetch.calls) != 5 {
		t.Errorf("拉取交易日数 = %d, 期望 5 (calls=%v)", len(fetch.calls), fetch.calls)
	}
	if synced != 5 {
		t.Errorf("同步数 = %d, 期望 5", synced)
	}
	if attempts != 5 {
		t.Errorf("尝试数 = %d, 期望 5 (已同步日期应被跳过, 不计入尝试)", attempts)
	}
	if storeCalls.Load() != 5 {
		t.Errorf("入库次数 = %d, 期望 5", storeCalls.Load())
	}
}

func TestSyncByTradeDayStoreError(t *testing.T) {
	repo := &fakeMaxDateRepo{} // maxDate 为空: 全部拉取
	fetch := &fakeFetch{}

	cals := tradeCals(5)
	synced, attempts := syncByTradeDay(repo, cals, "测试", fetch.fn, func(items []int) error {
		return fmt.Errorf("模拟入库失败")
	})

	if synced != 0 {
		t.Errorf("全部入库失败时同步数 = %d, 期望 0", synced)
	}
	if attempts != 5 {
		t.Errorf("尝试数 = %d, 期望 5", attempts)
	}
	if daySyncError(synced, attempts) == nil {
		t.Errorf("有尝试且零成功应判定为失败")
	}
	if len(fetch.calls) != 5 {
		t.Errorf("入库失败也应拉取全部: calls=%v", fetch.calls)
	}
}

func TestSyncByTradeDayFetchError(t *testing.T) {
	repo := &fakeMaxDateRepo{}
	cals := tradeCals(5)
	synced, attempts := syncByTradeDay(repo, cals, "测试", func(calDate string) ([]int, error) {
		return nil, fmt.Errorf("模拟拉取失败")
	}, func(items []int) error { return nil })

	if synced != 0 {
		t.Errorf("全部拉取失败时同步数 = %d, 期望 0", synced)
	}
	if daySyncError(synced, attempts) == nil {
		t.Errorf("接口整体不可用应判定为失败")
	}
}

// TestSyncByTradeDayEmptyResult 当日无数据属同步成功但不入库
// 计为成功是必须的: 若按失败计, 只持 ETF 的账户龙虎榜长期空行会让 daySyncError 天天误报
func TestSyncByTradeDayEmptyResult(t *testing.T) {
	repo := &fakeMaxDateRepo{}
	var storeCalls atomic.Int64
	cals := tradeCals(5)
	synced, attempts := syncByTradeDay(repo, cals, "测试", func(calDate string) ([]int, error) {
		return nil, nil // 空结果
	}, func(items []int) error {
		storeCalls.Add(1)
		return nil
	})

	if synced != 5 {
		t.Errorf("空结果应计为同步成功: synced=%d, 期望 5", synced)
	}
	if storeCalls.Load() != 0 {
		t.Errorf("空结果不应触发入库, storeCalls=%d", storeCalls.Load())
	}
	if err := daySyncError(synced, attempts); err != nil {
		t.Errorf("全为空数据但接口正常, 不应判定为失败: %v", err)
	}
}

func TestSyncByTradeDayConcurrencySafety(t *testing.T) {
	// 大量交易日并发执行 (配合 -race 检测 fetch/store 的数据竞争)
	repo := &fakeMaxDateRepo{}
	var fetchCalls atomic.Int64
	var storeCalls atomic.Int64
	cals := tradeCals(200)
	synced, _ := syncByTradeDay(repo, cals, "测试",
		func(calDate string) ([]int, error) {
			fetchCalls.Add(1)
			return []int{1}, nil
		},
		func(items []int) error {
			storeCalls.Add(1)
			return nil
		})

	if synced != 200 || fetchCalls.Load() != 200 || storeCalls.Load() != 200 {
		t.Errorf("并发同步不完整: synced=%d fetch=%d store=%d", synced, fetchCalls.Load(), storeCalls.Load())
	}
}
