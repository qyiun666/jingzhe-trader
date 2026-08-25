package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestTradePlanStatusFlow trade_plan 状态流转: pending → confirmed → executed / expired
func TestTradePlanStatusFlow(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	repo := NewPlanRepo(db)
	id, err := repo.InsertPlan(&TradePlan{
		TradeDate: "20260102", TsCode: "600519.SH", Direction: "buy",
		Qty: 100, RefPrice: 1400, Reason: "测试",
	})
	if err != nil {
		t.Fatalf("插入计划失败: %v", err)
	}

	plan, err := repo.GetPlanByID(id)
	if err != nil {
		t.Fatalf("查询计划失败: %v", err)
	}
	if plan.Status != PlanStatusPending {
		t.Errorf("新计划状态应为 pending, 实际 %s", plan.Status)
	}

	// pending → confirmed → executed
	if err := repo.UpdatePlanStatus(id, PlanStatusConfirmed); err != nil {
		t.Fatalf("确认计划失败: %v", err)
	}
	if err := repo.UpdatePlanStatus(id, PlanStatusExecuted); err != nil {
		t.Fatalf("执行计划失败: %v", err)
	}
	plan, _ = repo.GetPlanByID(id)
	if plan.Status != PlanStatusExecuted {
		t.Errorf("计划状态应为 executed, 实际 %s", plan.Status)
	}

	// 旧日期 pending 计划过期; 已 executed 的不受影响
	oldID, _ := repo.InsertPlan(&TradePlan{
		TradeDate: "20251230", TsCode: "000001.SZ", Direction: "sell", Qty: 200,
	})
	n, err := repo.ExpireOldPlans("20260102")
	if err != nil {
		t.Fatalf("过期旧计划失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应过期 1 条计划, 实际 %d", n)
	}
	oldPlan, _ := repo.GetPlanByID(oldID)
	if oldPlan.Status != PlanStatusExpired {
		t.Errorf("旧计划应为 expired, 实际 %s", oldPlan.Status)
	}

	// GetOpenPlans 只返回 pending/confirmed
	openID, _ := repo.InsertPlan(&TradePlan{
		TradeDate: "20260102", TsCode: "000002.SZ", Direction: "buy", Qty: 300,
	})
	open, err := repo.GetOpenPlans()
	if err != nil {
		t.Fatalf("查询待处理计划失败: %v", err)
	}
	if len(open) != 1 || open[0].ID != openID {
		t.Errorf("待处理计划应只有 1 条(id=%d), 实际 %d 条", openID, len(open))
	}
}

// TestReplaceDayPlans EOD重跑覆盖: 只清 pending+normal, urgent 与已确认计划保留
func TestReplaceDayPlans(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	repo := NewPlanRepo(db)
	date := "20260102"
	repo.InsertPlan(&TradePlan{TradeDate: date, TsCode: "A.SZ", Direction: "buy", Qty: 100})
	urgentID, _ := repo.InsertPlan(&TradePlan{
		TradeDate: date, TsCode: "B.SZ", Direction: "sell", Qty: 100, Urgency: PlanUrgencyUrgent,
	})
	confirmedID, _ := repo.InsertPlan(&TradePlan{TradeDate: date, TsCode: "C.SZ", Direction: "buy", Qty: 100})
	repo.UpdatePlanStatus(confirmedID, PlanStatusConfirmed)

	if err := repo.ReplaceDayPlans(date, []*TradePlan{
		{TradeDate: date, TsCode: "D.SZ", Direction: "buy", Qty: 200},
	}); err != nil {
		t.Fatalf("覆盖当日计划失败: %v", err)
	}

	plans, _ := repo.GetPlansByDate(date)
	if len(plans) != 3 {
		t.Fatalf("覆盖后应有 3 条计划(urgent+confirmed+新计划), 实际 %d", len(plans))
	}
	codes := map[string]bool{}
	for _, p := range plans {
		codes[p.TsCode] = true
	}
	if codes["A.SZ"] || !codes["B.SZ"] || !codes["C.SZ"] || !codes["D.SZ"] {
		t.Errorf("覆盖结果错误: %v (A应被清理, B/C/D应保留)", codes)
	}
	_ = urgentID
}

// TestRunRetention 数据清理: 只删过期数据, live_ 前缀回测run永久保留
func TestRunRetention(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	oldDate := time.Now().AddDate(-4, 0, 0).Format("20060102")    // 4年前, 超出3年保留期
	recentDate := time.Now().AddDate(0, 0, -1).Format("20060102") // 昨天

	// 行情: 一条过期 + 一条新鲜
	db.MustExec(`INSERT INTO daily_bar (ts_code, trade_date, close) VALUES (?, ?, 10)`, "600519.SH", oldDate)
	db.MustExec(`INSERT INTO daily_bar (ts_code, trade_date, close) VALUES (?, ?, 11)`, "600519.SH", recentDate)

	// 新闻: 过期 + 新鲜
	oldNews := time.Now().AddDate(0, 0, -60).Format("2006-01-02 15:04:05")
	newNews := time.Now().Format("2006-01-02 15:04:05")
	db.MustExec(`INSERT INTO news (datetime, content, title, channels) VALUES (?, 'x', 'old', '')`, oldNews)
	db.MustExec(`INSERT INTO news (datetime, content, title, channels) VALUES (?, 'x', 'new', '')`, newNews)

	// 回测run: 3个bt run + 1个live run, 保留最近2个bt
	for _, runID := range []string{"bt_1", "bt_2", "bt_3", "live_main"} {
		db.MustExec(`INSERT INTO trades (run_id, ts_code, side, price, qty, trade_date) VALUES (?, 'A.SZ', 1, 10, 100, ?)`,
			runID, recentDate)
		db.MustExec(`INSERT INTO account_snapshot (run_id, trade_date, total_asset) VALUES (?, ?, 10000)`,
			runID, recentDate)
	}

	// 交易计划: 过期 + 新鲜
	oldPlanDate := time.Now().AddDate(0, 0, -120).Format("20060102")
	db.MustExec(`INSERT INTO trade_plan (trade_date, ts_code, name, direction, qty, ref_price, reason, strategy, urgency, status, created_at, updated_at)
		VALUES (?, 'A.SZ', '', 'buy', 100, 10, '', '', 'normal', 'expired', '', '')`, oldPlanDate)
	db.MustExec(`INSERT INTO trade_plan (trade_date, ts_code, name, direction, qty, ref_price, reason, strategy, urgency, status, created_at, updated_at)
		VALUES (?, 'B.SZ', '', 'buy', 100, 10, '', '', 'normal', 'pending', '', '')`, recentDate)

	policy := RetentionPolicy{BarYears: 3, NewsDays: 30, PlanDays: 90, BacktestRuns: 2}
	if err := RunRetention(db, policy, true); err != nil {
		t.Fatalf("执行数据清理失败: %v", err)
	}

	assertCount := func(query, desc string, want int) {
		t.Helper()
		var n int
		if err := db.Get(&n, query); err != nil {
			t.Fatalf("查询 %s 失败: %v", desc, err)
		}
		if n != want {
			t.Errorf("%s: 期望 %d 行, 实际 %d 行", desc, want, n)
		}
	}
	assertCount(`SELECT COUNT(1) FROM daily_bar`, "daily_bar", 1)
	assertCount(`SELECT COUNT(1) FROM news`, "news", 1)
	assertCount(`SELECT COUNT(1) FROM trades WHERE run_id = 'live_main'`, "live run trades", 1)
	assertCount(`SELECT COUNT(1) FROM trades WHERE run_id = 'bt_1'`, "最旧bt run应被清理", 0)
	assertCount(`SELECT COUNT(1) FROM trades WHERE run_id LIKE 'bt_%'`, "保留的bt run trades", 2)
	assertCount(`SELECT COUNT(1) FROM trade_plan`, "trade_plan", 1)
}
