package store

import "jingzhe-trader/internal/model"

// boolToInt 布尔转 0/1（SQLite 无原生布尔）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullFen Fen → int64（未设置时为 0，调用方保证语义合理）。
func nullFen(f model.Fen) int64 {
	return int64(f)
}

// nullInt64 原样返回（保持接口一致，便于后续替换为 sql.NullInt64）。
func nullInt64(v int64) int64 {
	return v
}
