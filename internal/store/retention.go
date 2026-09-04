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
	Days      int    // 窗口（天），>0 时按 trade_date < now-Days 清理
	Permanent bool   // 永久保留，不清理
	// KeyPrefix 非空 = 清理 config_kv 里"一天一键"的集合行：键形如 suspend:<YYYYMMDD>，
	// 后缀字典序即时间序，按键区间比较即可只命中这一类键（这类表没有 trade_date 列）。
	KeyPrefix string
}

// RetentionRules 全部保留策略（与 §3.9 一一对应）。
var RetentionRules = []RetentionRule{
	// 窗口按"最深消费者"定：
	//   daily_bar —— 选股因子窗口 20 个交易日（同步侧保证 25 天），45 自然日 ≈ 30 交易日；
	//               指数与个股共用本表，MA20 的 20 日回溯同样落在这个窗口内。
	//   估值截面（stock_basic 的 val_date 列）与持仓同键，随 stock_basic 永久保留、
	//               每日整批覆盖，不设窗口 —— 原 daily_basic 表 16.6K 行/天的堆积没有了。
	//   run_trace —— 取代 job_run/agent_alert/action_log/mail_outbox/llm_call，按最深的
	//               消费者（月度复盘看当日成败）留 90 天；LLM 留痕同窗口，不再单独配键。
	{Table: "daily_bar", ConfigKey: "retention.bar_days", Days: 45},
	// 停牌集合挤进了 config_kv，只能按键区间清；它和估值截面一样"当日整批读一次"，
	// 留 3 天（多出的 2 天是跨天重跑的余量）。
	{Table: "config_kv", ConfigKey: "retention.suspend_days", Days: 3, KeyPrefix: "suspend:"},
	{Table: "run_trace", ConfigKey: "retention.trace_days", Days: 90},
	// 永久保留（结果与状态，行数天然有限）：
	// trade_cal / stock_basic / order_ticket / position
	// config_kv 整体永久，只有机器写入的 suspend:<日期> 集合按日清理（见 KeyPrefix 规则）
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
		// pick 取窗口配置覆盖：未配置或非正值时沿用规则默认窗口。
		pick := func(def int) int {
			if v, ok := overrides[rule.ConfigKey]; ok && v > 0 {
				return v
			}
			return def
		}
		var where string
		var args []interface{}
		switch {
		case rule.KeyPrefix != "":
			cutoff := now.AddDate(0, 0, -pick(rule.Days)).Format("20060102")
			where, args = "key >= ? AND key < ?",
				[]interface{}{rule.KeyPrefix + "00000000", rule.KeyPrefix + cutoff}
		case rule.Days > 0:
			cutoff := now.AddDate(0, 0, -pick(rule.Days)).Format("20060102")
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
	stmt := "PRAGMA incremental_vacuum" // 不带数字 = 回收全部空闲页
	if pages > 0 {
		stmt = fmt.Sprintf("PRAGMA incremental_vacuum(%d)", pages)
	}
	if _, err := s.writeDB.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("增量 vacuum 失败: %w", err)
	}
	return nil
}
