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
			"llm.search_context_size": {Key: "llm.search_context_size", Value: "high"},
			"screen.factor_mode":      {Key: "screen.factor_mode", Value: "reversal"},
			"screen.gate_ma_window":   {Key: "screen.gate_ma_window", Value: window},
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
