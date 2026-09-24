package scheduler

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
)

// fakeConfig 最小 ConfigReader：键缺失返回空串，与老库（无新键）的读取行为一致。
type fakeConfig map[string]string

func (f fakeConfig) GetString(key string) string { return f[key] }

// TestMarketGateOf 门槛配置的解析口径：
// 老库缺键 → 默认"开 + MA60"（与参数化之前的编译期常量行为逐位一致）；
// 坏值 / 越界窗口 → 钳回保守默认（开闸判定、默认深度），而不是炸链或悄悄放行。
func TestMarketGateOf(t *testing.T) {
	cases := []struct {
		name        string
		cfg         fakeConfig
		wantEnabled bool
		wantWindow  int
	}{
		{"老库缺键", fakeConfig{}, true, store.MarketMAWindow},
		{"关闸", fakeConfig{"screen.gate_enabled": "false", "screen.gate_ma_window": "20"}, false, 20},
		{"窗口越上界", fakeConfig{"screen.gate_enabled": "true", "screen.gate_ma_window": "999"}, true, store.MarketMAWindow},
		{"窗口为 0", fakeConfig{"screen.gate_enabled": "true", "screen.gate_ma_window": "0"}, true, store.MarketMAWindow},
		{"窗口坏值", fakeConfig{"screen.gate_enabled": "true", "screen.gate_ma_window": "abc"}, true, store.MarketMAWindow},
		{"开关坏值", fakeConfig{"screen.gate_enabled": "yes-please", "screen.gate_ma_window": "60"}, true, 60},
	}
	for _, c := range cases {
		got := MarketGateOf(c.cfg)
		if got.Enabled != c.wantEnabled || got.Window != c.wantWindow {
			t.Errorf("%s: 期望 {Enabled:%v Window:%d}，实际 %+v",
				c.name, c.wantEnabled, c.wantWindow, got)
		}
	}
}

// TestEmptyReasonCarriesConfiguredWindow 关闸文案里的均线名必须跟着报告里的窗口走：
// 窗口配成 20 还写"跌破 MA60"，日报就是在对用户撒谎。
func TestEmptyReasonCarriesConfiguredWindow(t *testing.T) {
	rep := &screener.Report{RegimeClosed: true, RegimeMA: 20}
	got := emptyReason(rep)
	if !strings.Contains(got, "MA20") {
		t.Errorf("文案应回显配置窗口 MA20，实际: %q", got)
	}
	if strings.Contains(got, "MA60") {
		t.Errorf("窗口为 20 时不应再出现写死的 MA60，实际: %q", got)
	}
}
