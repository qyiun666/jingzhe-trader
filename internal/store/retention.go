package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// RetentionRule 单表保留策略（ARCHITECTURE §3.9）。
type RetentionRule struct {
	Table     string // 表名（白名单常量）
	ConfigKey string // 对应 retention.* 配置键
	Years     int    // 窗口（年），>0 时按 trade_date < now-Years 清理
	Days      int    // 窗口（天），>0 时按 trade_date < now-Days 清理
	Quarters  int    // 保留报告期数（仅 fina_indicator 用，保留最近 N 个 end_date）
	Permanent bool   // 永久保留，不清理
}

// RetentionRules 全部保留策略（与 §3.9 一一对应）。
var RetentionRules = []RetentionRule{
	{Table: "daily_bar", ConfigKey: "retention.bar_years", Years: 3},
	{Table: "daily_basic", ConfigKey: "retention.bar_years", Years: 3},
	{Table: "stk_limit", ConfigKey: "retention.bar_years", Years: 3},
	{Table: "suspend_d", ConfigKey: "retention.bar_years", Years: 3},
	{Table: "adj_factor", ConfigKey: "retention.bar_years", Years: 3},
	{Table: "index_daily", ConfigKey: "retention.bar_years", Years: 3},
	{Table: "fina_indicator", Quarters: 8}, // 保留最近 8 个报告期
	{Table: "moneyflow", ConfigKey: "retention.mf_days", Days: 60},
	{Table: "screen_result", ConfigKey: "retention.screen_days", Days: 90},
	{Table: "signal", ConfigKey: "retention.signal_days", Days: 365},
	{Table: "agent_alert", ConfigKey: "retention.alert_days", Days: 180},
	{Table: "job_run", ConfigKey: "retention.job_days", Days: 90},
	{Table: "mail_outbox", ConfigKey: "retention.mail_days", Days: 30},
	{Table: "llm_call", ConfigKey: "retention.llm_days", Days: 90},
	// order_ticket / fill / goal_gear_log / action_log / position / account_snapshot / goal_state：永久
}

// ApplyRetention 按策略分批清理各表（§3.9 三条硬约束：分批 + 让锁 + 总耗时上限）。
// overrides 为 retention.* 配置覆盖（键→天数/年数）。返回各表删除行数。
// 若任一表因超时（context.DeadlineExceeded）未完成，返回剩余结果并附超时错误（调用方应记 degraded）。
func ApplyRetention(ctx context.Context, s *Store, now time.Time, overrides map[string]int) (map[string]int, error) {
	results := make(map[string]int)
	var firstErr error
	for _, rule := range RetentionRules {
		if rule.Permanent {
			continue
		}
		var where string
		var args []interface{}
		switch {
		case rule.Table == "fina_indicator":
			// 保留最近 8 个报告期（按 end_date 降序跳过前 8）
			where = "end_date < (SELECT end_date FROM fina_indicator ORDER BY end_date DESC LIMIT 1 OFFSET 7)"
		case rule.Years > 0:
			days := rule.Years * 365
			if v, ok := overrides[rule.ConfigKey]; ok && v > 0 {
				days = v * 365
			}
			cutoff := now.AddDate(0, 0, -days).Format("20060102")
			where, args = "trade_date < ?", []interface{}{cutoff}
		case rule.Days > 0:
			d := rule.Days
			if v, ok := overrides[rule.ConfigKey]; ok && v > 0 {
				d = v
			}
			cutoff := now.AddDate(0, 0, -d).Format("20060102")
			where, args = "trade_date < ?", []interface{}{cutoff}
		default:
			continue
		}
		deleted, _, err := DeleteBatched(ctx, s.writeDB, rule.Table, where, args, DefaultBatchDeleteLimit)
		results[rule.Table] = deleted
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				if firstErr == nil {
					firstErr = fmt.Errorf("清理 %s 超时，保留剩余: %w", rule.Table, err)
				}
				break // 超时：停止后续表清理，保留剩余到下一天
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("清理 %s 失败: %w", rule.Table, err)
			}
		}
	}
	return results, firstErr
}

// WALCheckpoint 每日 WAL checkpoint(TRUNCATE)，归还空间（§3.9）。
func WALCheckpoint(ctx context.Context, s *Store) error {
	if _, err := s.writeDB.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("WAL checkpoint 失败: %w", err)
	}
	return nil
}

// IncrementalVacuum 每周日增量回收空间（分次，避免一次性长事务，§3.9）。
func IncrementalVacuum(ctx context.Context, s *Store, pages int) error {
	if pages <= 0 {
		pages = 1000
	}
	if _, err := s.writeDB.ExecContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages)); err != nil {
		return fmt.Errorf("增量 vacuum 失败: %w", err)
	}
	return nil
}
