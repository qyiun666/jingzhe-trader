package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// TimeLayout 数据库时间戳格式 (统一全包时间字面量, 避免各 repo 硬编码)
const TimeLayout = "2006-01-02 15:04:05"

// isNoRowsErr 判断是否为"无数据"错误(database/sql.ErrNoRows)
func isNoRowsErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows)
}

// sqlxGetter sqlx 查询执行器 (DB/Tx 通用)
type sqlxGetter interface {
	Get(dest interface{}, query string, args ...interface{}) error
	Select(dest interface{}, query string, args ...interface{}) error
}

// batchInsert 事务批量插入: 预编译 insertSQL 后逐行执行 execFn
// n 为插入行数; 返回错误时事务自动回滚
func batchInsert(db *sqlx.DB, insertSQL, errDesc string, n int, execFn func(stmt *sqlx.Stmt, i int) error) error {
	if n == 0 {
		return nil
	}
	tx, err := db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(insertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for i := 0; i < n; i++ {
		if err := execFn(stmt, i); err != nil {
			return fmt.Errorf("%s: %w", errDesc, err)
		}
	}
	return tx.Commit()
}

// selectList 列表查询 + 统一错误包装
func selectList(db sqlxGetter, query string, dest any, errMsg string, args ...any) error {
	if err := db.Select(dest, query, args...); err != nil {
		return fmt.Errorf("%s: %w", errMsg, err)
	}
	return nil
}

// getOne 单条查询: 无记录返回 (false, nil), 其余错误统一包装
func getOne(db sqlxGetter, query string, dest any, errMsg string, args ...any) (bool, error) {
	if err := db.Get(dest, query, args...); err != nil {
		if isNoRowsErr(err) {
			return false, nil
		}
		return false, fmt.Errorf("%s: %w", errMsg, err)
	}
	return true, nil
}

// maxTableDate 查询表的最大 trade_date
func maxTableDate(db sqlxGetter, table string) (string, error) {
	var d string
	query := fmt.Sprintf(`SELECT MAX(trade_date) FROM %s`, table)
	if err := db.Get(&d, query); err != nil {
		return "", fmt.Errorf("查询最大交易日失败: %w", err)
	}
	return d, nil
}

// existsRow 存在性检查: COUNT(1) > 0
func existsRow(db sqlxGetter, query string, args ...any) (bool, error) {
	var n int
	if err := db.Get(&n, query, args...); err != nil {
		return false, fmt.Errorf("存在性检查失败: %w", err)
	}
	return n > 0, nil
}
