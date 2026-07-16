package store

import (
	"database/sql"
	"errors"
)

// isNoRowsErr 判断是否为"无数据"错误(database/sql.ErrNoRows)
func isNoRowsErr(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sql.ErrNoRows)
}
