package observability

import (
	"context"
	"testing"
)

// TestRunCtxArtifactContract 对应验收 #10：
// 声明 5 个产出物、实际登记 3 个（其中 1 个必产出未登记），
// Assert 应得到恰好 1 条 ARTIFACT_MISSING（code=ARTIFACT_MISSING），且 Degraded() 为 true。
func TestRunCtxArtifactContract(t *testing.T) {
	rc := NewRunCtx(context.Background(), "daily_screen", "20260331")

	// 声明 5 个产出物
	rc.Declare("signal", "buy", 1)    // 必产出，已满足
	rc.Declare("signal", "sell", 1)   // 必产出，已满足
	rc.Declare("report", "daily", 1)  // 必产出，已满足
	rc.Declare("report", "weekly", 1) // 必产出，未登记 → 缺失
	rc.Declare("metric", "health", 0) // 可选（expect=0），不校验

	// 实际登记 3 个（buy/sell/daily），均达到期望
	rc.Actual("buy", 1)
	rc.Actual("sell", 2)
	rc.Actual("daily", 1)

	misses := rc.Assert()
	if len(misses) != 1 {
		t.Fatalf("期望恰好 1 条 ARTIFACT_MISSING，实际 %d: %+v", len(misses), misses)
	}
	if misses[0].Code != ArtifactCode {
		t.Fatalf("缺失项 code 应为 %s，实际 %s", ArtifactCode, misses[0].Code)
	}
	if misses[0].Name != "weekly" {
		t.Fatalf("缺失项应为 weekly，实际 %s", misses[0].Name)
	}
	if misses[0].Expect != 1 || misses[0].Actual != 0 {
		t.Fatalf("缺失项期望/实际应为 1/0，实际 %d/%d", misses[0].Expect, misses[0].Actual)
	}
	if !rc.Degraded() {
		t.Fatal("Assert 发现缺失后 Degraded() 应为 true")
	}
}

// TestRunCtxDegrade 对应验收 #10：Degrade 是唯一跳过出口，调用后 Degraded()=true 并记录原因。
func TestRunCtxDegrade(t *testing.T) {
	rc := NewRunCtx(context.Background(), "sync", "20260331")
	rc.Declare("data", "cal", 1)
	rc.Degrade("NET_TIMEOUT", "tushare 不可达，跳过交易日历同步")

	if !rc.Degraded() {
		t.Fatal("Degrade 后 Degraded() 应为 true")
	}
	degs := rc.Degradations()
	if len(degs) != 1 {
		t.Fatalf("期望 1 条降级记录，实际 %d", len(degs))
	}
	if degs[0].Code != "NET_TIMEOUT" || degs[0].Reason == "" {
		t.Fatalf("降级记录不正确: %+v", degs[0])
	}

	// Degrade 不影响 Assert：已满足的产出物不应再产生缺失
	rc.Actual("cal", 1)
	if miss := rc.Assert(); len(miss) != 0 {
		t.Fatalf("cal 已满足，不应有缺失: %+v", miss)
	}
	if !rc.Degraded() {
		t.Fatal("已 Degrade 的任务应保持 degraded")
	}
}

// TestRunCtxActualPanic 未 Declare 就 Actual 属编程错误（必须 panic）。
func TestRunCtxActualPanic(t *testing.T) {
	rc := NewRunCtx(context.Background(), "j", "d")
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("未 Declare 就 Actual 应当 panic")
		}
	}()
	rc.Actual("undeclared", 1)
}

// TestRunCtxAtLeastOne expect=-1 表示"至少 1 个"，缺则报缺失。
func TestRunCtxAtLeastOne(t *testing.T) {
	rc := NewRunCtx(context.Background(), "j", "d")
	rc.Declare("report", "daily", -1) // 至少 1 个

	// 未登记任何实际产出 → 应报告缺失
	misses := rc.Assert()
	if len(misses) != 1 {
		t.Fatalf("expect=-1 且未产出应报 1 条缺失，实际 %d", len(misses))
	}

	// 登记 1 个后 → 满足
	rc.Actual("daily", 1)
	if miss := rc.Assert(); len(miss) != 0 {
		t.Fatalf("登记 1 个后应满足，仍有缺失: %+v", miss)
	}
}
