package tushare

import "testing"

// stock_basic 无 is_st 列, ST 判定只能按命名规则从名称推导
func TestIsSTName(t *testing.T) {
	cases := map[string]bool{
		"*ST美丽": true,
		"ST海王":  true,
		"sT晨鸣":  true,
		" ST中迪": true,
		"平安银行":  false,
		"TCL科技": false,
		"宁德时代":  false,
		"":      false,
		"*ST华通": true,
		"退市金鈺":  false,
		"ST":    true,
		"普联软件":  false,
		"西陇科学":  false,
		"哈三联":   false,
		"佳缘科技":  false,
		"众信旅游":  false,
	}
	for name, want := range cases {
		if got := isSTName(name); got != want {
			t.Errorf("isSTName(%q) = %v, want %v", name, got, want)
		}
	}
}
