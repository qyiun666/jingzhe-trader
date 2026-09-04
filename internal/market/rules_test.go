package market

import "testing"

// TestIsSTName ST 判定只看名称前缀：库里不再存 is_st 列，名称就是唯一真相源。
// 戴帽（ST/*ST）、退市整理都要挡住；把"沙隆达/ST"这类只含字母 S 的正常名字放过去。
func TestIsSTName(t *testing.T) {
	st := []string{
		"ST某某", "ST 某某", "*ST光一", "*ST某某", "SST华新", "S*ST聚友",
		"某某退", "退市整理", "ST赛为",
	}
	for _, name := range st {
		if !IsSTName(name) {
			t.Errorf("IsSTName(%q) = false，期望 true", name)
		}
	}
	ok := []string{
		"平安银行", "贵州茅台", "S佳通", "沙隆达A", "中芯国际",
		"招商银行", "宁德时代", "苏泊尔",
	}
	for _, name := range ok {
		if IsSTName(name) {
			t.Errorf("IsSTName(%q) = true，期望 false", name)
		}
	}
}
