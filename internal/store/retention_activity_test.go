package store

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRunRetentionCoversActivityTables 此前无保留策略的表必须被纳入清理, 且只能删超期数据。
// 背景: action_log 由每个调度任务每笔写入(盘中监控 5 分钟一次), 不清理会成为库里最大的表。
func TestRunRetentionCoversActivityTables(t *testing.T) {
	db, err := NewDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化测试库失败: %v", err)
	}
	defer db.Close()

	daysAgo := func(n int) string { return time.Now().AddDate(0, 0, -n).Format("20060102") }
	now := time.Now().Format("2006-01-02 15:04:05")
	// 保留期边界内外的样本: ActionDays/AlertDays/ScreenDays=90, DebateDays=180, BarYears=3
	expired, inside, graceInside := daysAgo(91), daysAgo(89), daysAgo(95)

	db.MustExec(`INSERT INTO action_log (trade_date, kind, name, status, created_at) VALUES (?, 'task', 'old', 'success', ?)`, expired, now)
	db.MustExec(`INSERT INTO action_log (trade_date, kind, name, status, created_at) VALUES (?, 'task', 'new', 'success', ?)`, inside, now)

	// 告警: 超期已读应删; 超期但未读在宽限期内应留; 远超宽限期即使未读也删
	db.MustExec(`INSERT INTO agent_alert (trade_date, job_name, title, content, status, created_at) VALUES (?, 'x','t','c','read', ?)`, expired, now)
	db.MustExec(`INSERT INTO agent_alert (trade_date, job_name, title, content, status, created_at) VALUES (?, 'x','t','c','unread', ?)`, graceInside, now)
	db.MustExec(`INSERT INTO agent_alert (trade_date, job_name, title, content, status, created_at) VALUES (?, 'x','t','c','unread', ?)`, daysAgo(200), now)
	db.MustExec(`INSERT INTO agent_alert (trade_date, job_name, title, content, status, created_at) VALUES (?, 'x','t','c','unread', ?)`, inside, now)

	db.MustExec(`INSERT INTO screen_result (ts_code, trade_date, score) VALUES ('A.SH', ?, 1)`, expired)
	db.MustExec(`INSERT INTO screen_result (ts_code, trade_date, score) VALUES ('B.SH', ?, 1)`, inside)

	oldBar := time.Now().AddDate(-4, 0, 0).Format("20060102")
	db.MustExec(`INSERT INTO top_list (ts_code, trade_date, name) VALUES ('A.SH', ?, 'x')`, oldBar)
	db.MustExec(`INSERT INTO top_list (ts_code, trade_date, name) VALUES ('A.SH', ?, 'x')`, inside)

	db.MustExec(`INSERT INTO agent_debate (trade_date, ts_code, decision, created_at) VALUES (?, 'A.SH', 'buy', ?)`, expired, now)
	db.MustExec(`INSERT INTO agent_debate (trade_date, ts_code, decision, created_at) VALUES (?, 'A.SH', 'buy', ?)`, daysAgo(181), now)

	// 财务指标: 5 个报告期, 只留最近 3 个
	for _, end := range []string{"20241231", "20250331", "20250630", "20250930", "20251231"} {
		db.MustExec(`INSERT INTO fina_indicator (ts_code, end_date, eps) VALUES ('A.SH', ?, 1)`, end)
	}

	// 整棵配置与运行态资金都在 config_kv, 清理绝不能波及这两类行
	db.MustExec(`INSERT INTO config_kv (key, value) VALUES ('config', '{"screener":{"max_pe":50}}')`)
	db.MustExec(`INSERT INTO config_kv (key, value) VALUES ('cash', '10320.30')`)

	policy := RetentionPolicy{
		BarYears: 3, AlertDays: 90, ActionDays: 90,
		ScreenDays: 90, DebateDays: 180, FinaQuarters: 3,
	}
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
	assertCount(`SELECT COUNT(1) FROM action_log`, "action_log 只留未超期", 1)
	assertCount(`SELECT COUNT(1) FROM agent_alert`, "agent_alert 应删掉超期已读+远超期未读, 留宽限期内未读与未超期", 2)
	assertCount(`SELECT COUNT(1) FROM agent_alert WHERE status='unread'`, "unread 告警保留数", 2)
	assertCount(`SELECT COUNT(1) FROM screen_result`, "screen_result 只留未超期", 1)
	assertCount(`SELECT COUNT(1) FROM top_list`, "top_list 只留未超期", 1)
	assertCount(`SELECT COUNT(1) FROM agent_debate`, "agent_debate 按 180 天窗口只删掉 181 天前那条", 1)

	// 财务指标: 5 个报告期只留最近 3 个 (20250630 / 20250930 / 20251231)
	assertCount(`SELECT COUNT(DISTINCT end_date) FROM fina_indicator`, "fina_indicator 保留报告期数", 3)
	assertCount(`SELECT COUNT(1) FROM fina_indicator WHERE end_date < '20250630'`, "更早报告期应已清除", 0)

	// 配置文档与运行态资金同在 config_kv, 清理不得波及
	assertCount(`SELECT COUNT(1) FROM config_kv WHERE key IN ('config','cash')`, "config_kv 行数", 2)
}
