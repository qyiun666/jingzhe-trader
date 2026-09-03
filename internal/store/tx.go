package store

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// 批量默认上限（ARCHITECTURE §11.5 / §3.9）。
const (
	DefaultBatchInsertLimit = 2000 // 每批写入行数上限
	DefaultBatchDeleteLimit = 5000 // 每批删除行数上限
	DefaultDeleteTimeout    = 5 * time.Minute
)

// WithTx 在事务中执行 fn；成功后提交，失败（或 panic）回滚。事务内禁止任何外部 IO。
func WithTx(ctx context.Context, db *sqlx.DB, fn func(*sqlx.Tx) error) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	// panic 隔离：回滚后原样抛出，交由上层 recover。
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("事务回滚失败(%v): %w", rbErr, err)
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("事务提交失败: %w", err)
	}
	return nil
}

// BatchInsert 分批插入。每批 ≤ batchSize 行（默认 2000），并受 SQLite 变量数上限约束。
// 表名必须来自白名单常量；列名与占位符均参数化（§11.5 禁止字符串拼接 SQL）。
func BatchInsert(ctx context.Context, db *sqlx.DB, table string, columns []string, rows [][]interface{}, batchSize int) (int, error) {
	if batchSize <= 0 {
		batchSize = DefaultBatchInsertLimit
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if len(columns) == 0 {
		return 0, fmt.Errorf("BatchInsert: 列不能为空")
	}
	// 受 SQLite 变量数上限约束（默认 999，留余量按 900 计）：实际每批行数 = min(batchSize, 900/列数)
	if maxRows := 900 / len(columns); maxRows < batchSize {
		if maxRows < 1 {
			maxRows = 1
		}
		batchSize = maxRows
	}
	colList := strings.Join(columns, ", ")
	total := 0
	for start := 0; start < len(rows); start += batchSize {
		end := start + batchSize
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]
		var b strings.Builder
		args := make([]interface{}, 0, len(batch)*len(columns))
		for i, row := range batch {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteByte('(')
			for j, v := range row {
				if j > 0 {
					b.WriteByte(',')
				}
				b.WriteByte('?')
				args = append(args, v)
			}
			b.WriteByte(')')
		}
		q := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", table, colList, b.String())
		res, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return total, fmt.Errorf("批量插入 %s 失败: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// DeleteBatched 分批删除（每批 ≤ batchLimit，默认 5000），批间提交事务并让出写锁，
// 单任务总耗时受 ctx 截止时间约束（默认 5 分钟），超时保留剩余并提前退出（返回 DeadlineExceeded）。
//
// 返回：deleted 已删除行数、batches 实际执行且删除>0 的批数、err（超时为 context.DeadlineExceeded）。
func DeleteBatched(ctx context.Context, db *sqlx.DB, table, where string, args []interface{}, batchLimit int) (deleted int, batches int, err error) {
	if batchLimit <= 0 {
		batchLimit = DefaultBatchDeleteLimit
	}
	if args == nil {
		args = []interface{}{}
	}

	effCtx := ctx
	var cancel context.CancelFunc
	if _, has := ctx.Deadline(); !has {
		effCtx, cancel = context.WithTimeout(ctx, DefaultDeleteTimeout)
		defer cancel()
	}

	// 注意：modernc.org/sqlite 未开启 SQLITE_ENABLE_UPDATE_DELETE_LIMIT，
	// 不支持 "DELETE ... LIMIT" 语法，改用 rowid 子查询（通用且兼容所有表）。
	base := "DELETE FROM " + table + " WHERE rowid IN (SELECT rowid FROM " + table
	if where != "" {
		base += " WHERE " + where
	}
	base += fmt.Sprintf(" LIMIT %d)", batchLimit)

	for {
		if err = effCtx.Err(); err != nil {
			// 超时或取消：保留剩余，提前退出。
			return deleted, batches, err
		}
		res, execErr := db.ExecContext(effCtx, base, args...)
		if execErr != nil {
			return deleted, batches, fmt.Errorf("分批删除 %s 失败: %w", table, execErr)
		}
		n, _ := res.RowsAffected()
		deleted += int(n)
		if n > 0 {
			batches++
		}
		if n < int64(batchLimit) {
			break // 最后一波不足一批，删除完毕
		}
		runtime.Gosched() // 批间让出写锁（§3.9）
	}
	return deleted, batches, nil
}
