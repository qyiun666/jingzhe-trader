package store

// boolToInt 布尔转 0/1（SQLite 无原生布尔）。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
