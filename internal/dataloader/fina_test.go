package dataloader

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"jingzhe-trader/internal/model"
)

// fakeFinaProvider 假财务数据源：按固定有序代码返回固定财报，可控失败。
type fakeFinaProvider struct {
	codes    []string
	failCode string // 遇到该 ts_code 返回错误
}

func (f *fakeFinaProvider) StockBasic(ctx context.Context) ([]model.StockBasic, error) {
	out := make([]model.StockBasic, 0, len(f.codes))
	for _, c := range f.codes {
		out = append(out, model.StockBasic{TsCode: c})
	}
	return out, nil
}

func (f *fakeFinaProvider) FinaIndicator(ctx context.Context, tsCode string) ([]model.FinaIndicator, error) {
	if tsCode == f.failCode {
		return nil, errors.New("simulated fina error")
	}
	return []model.FinaIndicator{
		{TsCode: tsCode, EndDate: "20231231", AnnDate: "20240101", EPS: 1, ROE: 0.1},
	}, nil
}

// TestFinaSync_Resume 模拟 "kill + restart"：第一批 limit=50，第二批续传（limit=0）。
// 续传后 done==总数 证明从游标继续、已处理的未重做。
func TestFinaSync_Resume(t *testing.T) {
	st := openTestStore(t)
	codes := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		codes = append(codes, fmt.Sprintf("600%03d.SH", i))
	}
	prov := &fakeFinaProvider{codes: codes}

	// 第一批：处理 50 只后暂停（limit=50）
	s1 := NewFinaSyncer(st, prov)
	if err := s1.Run(context.Background(), 50); err != nil {
		t.Fatalf("第一批失败: %v", err)
	}
	st1, _ := st.FinaRepo().GetSyncState(context.Background())
	if st1.Done != 50 {
		t.Fatalf("第一批期望 done=50, 实际 %d", st1.Done)
	}
	if st1.CursorTsCode != codes[50] {
		t.Fatalf("第一批游标期望 %s, 实际 %s", codes[50], st1.CursorTsCode)
	}

	// 第二批：续传（limit=0 不限）
	s2 := NewFinaSyncer(st, prov)
	if err := s2.Run(context.Background(), 0); err != nil {
		t.Fatalf("第二批失败: %v", err)
	}
	st2, _ := st.FinaRepo().GetSyncState(context.Background())
	if st2.Done != 200 {
		t.Fatalf("续传后期望 done=200（从游标继续、无重做），实际 %d", st2.Done)
	}
	if st2.Status != finaStatusSuccess {
		t.Fatalf("期望 success, 实际 %s", st2.Status)
	}
}

// TestFinaSync_InterruptResume 模拟进程在运行中被取消（ctx 取消）：
// 中断后状态为 interrupted 且游标指向未处理股票；重启后完成且 done==总数。
type interruptProvider struct {
	inner  *fakeFinaProvider
	cancel context.CancelFunc
	after  int
	calls  int
}

func (p *interruptProvider) StockBasic(ctx context.Context) ([]model.StockBasic, error) {
	return p.inner.StockBasic(ctx)
}

func (p *interruptProvider) FinaIndicator(ctx context.Context, tsCode string) ([]model.FinaIndicator, error) {
	p.calls++
	if p.calls > p.after {
		p.cancel() // 触发中断（循环顶部 select 会捕获）
	}
	return p.inner.FinaIndicator(ctx, tsCode)
}

func TestFinaSync_InterruptResume(t *testing.T) {
	st := openTestStore(t)
	codes := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		codes = append(codes, fmt.Sprintf("600%03d.SH", i))
	}
	inner := &fakeFinaProvider{codes: codes}
	ctx, cancel := context.WithCancel(context.Background())
	prov := &interruptProvider{inner: inner, cancel: cancel, after: 40}

	s1 := NewFinaSyncer(st, prov)
	if err := s1.Run(ctx, 0); err == nil {
		t.Fatal("期望中断返回错误（ctx.Err）")
	}
	st1, _ := st.FinaRepo().GetSyncState(context.Background())
	if st1.Status != finaStatusInterrupted {
		t.Fatalf("期望 interrupted, 实际 %s", st1.Status)
	}

	// 重启续传（不受 ctx 取消影响）
	s2 := NewFinaSyncer(st, inner)
	if err := s2.Run(context.Background(), 0); err != nil {
		t.Fatalf("续传失败: %v", err)
	}
	st2, _ := st.FinaRepo().GetSyncState(context.Background())
	if st2.Done != 100 {
		t.Fatalf("续传后期望 done=100（无重做），实际 %d", st2.Done)
	}
	if st2.Status != finaStatusSuccess {
		t.Fatalf("期望 success, 实际 %s", st2.Status)
	}
}
