package app

import (
	"testing"

	"jingzhe-trader/internal/config"
)

// TestValidateEnumsGateWindow 门槛窗口的装配期校验：合法边界放行，越界拒绝启动。
// 越界窗口若放行，均线因日线不足而不可算，等于把"可配置"变成"每天定时整链失败"。
func TestValidateEnumsGateWindow(t *testing.T) {
	base := func(window string) *config.Config {
		return &config.Config{Values: map[string]config.ConfigValue{
			"llm.search_context_size":        {Key: "llm.search_context_size", Value: "high"},
			"screen.factor_mode":             {Key: "screen.factor_mode", Value: "reversal"},
			"screen.gate_ma_window":          {Key: "screen.gate_ma_window", Value: window},
			"screen.gate_off_max_total_pct":  {Key: "screen.gate_off_max_total_pct", Value: "0.20"},
			"screen.gate_off_max_single_pct": {Key: "screen.gate_off_max_single_pct", Value: "0.10"},
		}}
	}
	if err := validateEnums(base("20")); err != nil {
		t.Errorf("窗口 20 应放行，实际拒绝: %v", err)
	}
	if err := validateEnums(base("60")); err != nil {
		t.Errorf("窗口 60（默认深度上限）应放行，实际拒绝: %v", err)
	}
	for _, bad := range []string{"61", "0", "abc"} {
		if err := validateEnums(base(bad)); err == nil {
			t.Errorf("窗口 %q 非法却放行了", bad)
		}
	}
}

// TestValidateEnumsWeakCaps 弱势试探上限的装配期校验：越物理熔断、非正、
// 单票大于总仓，都要拒绝启动而不是运行期悄悄钳回。
func TestValidateEnumsWeakCaps(t *testing.T) {
	base := func(total, single string) *config.Config {
		return &config.Config{Values: map[string]config.ConfigValue{
			"llm.search_context_size":        {Key: "llm.search_context_size", Value: "high"},
			"screen.factor_mode":             {Key: "screen.factor_mode", Value: "reversal"},
			"screen.gate_ma_window":          {Key: "screen.gate_ma_window", Value: "60"},
			"screen.gate_off_max_total_pct":  {Key: "screen.gate_off_max_total_pct", Value: total},
			"screen.gate_off_max_single_pct": {Key: "screen.gate_off_max_single_pct", Value: single},
		}}
	}
	if err := validateEnums(base("0.20", "0.10")); err != nil {
		t.Errorf("默认试探上限应放行，实际拒绝: %v", err)
	}
	if err := validateEnums(base("0.95", "0.60")); err != nil {
		t.Errorf("恰好等于物理熔断应放行，实际拒绝: %v", err)
	}
	for _, tc := range []struct{ total, single, why string }{
		{"0.96", "0.10", "总仓越熔断"},
		{"0.20", "0.61", "单票越熔断"},
		{"0", "0.10", "总仓为零"},
		{"0.20", "-1", "单票为负"},
		{"0.10", "0.20", "单票大于总仓"},
		{"abc", "0.10", "总仓不可解析"},
	} {
		if err := validateEnums(base(tc.total, tc.single)); err == nil {
			t.Errorf("%s（total=%s single=%s）应拒绝却放行了", tc.why, tc.total, tc.single)
		}
	}
}
