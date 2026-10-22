package mcp

import (
	"context"
	"testing"
	"time"
)

// fakeJobRunner 记录 RunNamed 收到的 ctx 是否已被取消。
type fakeJobRunner struct {
	ctxErr chan error
}

func (f *fakeJobRunner) RunNamed(ctx context.Context, _, _, _ string) error {
	select {
	case <-time.After(20 * time.Millisecond):
		f.ctxErr <- ctx.Err()
		return nil
	case <-ctx.Done():
		f.ctxErr <- ctx.Err()
		return ctx.Err()
	}
}

func (f *fakeJobRunner) JobNames() []string { return []string{"slow"} }

// TestRunManualJobIgnoresHTTPCancel 回归 10-21：客户端 30s 断开不能把
// 已受理的补跑取消掉。父 ctx 已取消时，作业 ctx 仍应是活的。
func TestRunManualJobIgnoresHTTPCancel(t *testing.T) {
	jobs := &fakeJobRunner{ctxErr: make(chan error, 1)}
	s := &Server{deps: &Deps{Jobs: jobs}}

	parent, cancel := context.WithCancel(context.Background())
	cancel() // 模拟 HTTP 处理器因客户端断开而取消

	if err := s.runManualJob(parent, "slow", "20260901"); err != nil {
		t.Fatalf("父 ctx 取消不应传给作业: %v", err)
	}
	select {
	case err := <-jobs.ctxErr:
		if err != nil {
			t.Fatalf("作业 ctx 被父取消传染: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("作业未在预期时间内执行")
	}
}
