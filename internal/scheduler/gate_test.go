package scheduler

import (
	"context"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
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

// TestDetectWeakRegime 门槛关闭时的弱势判定（任务2 归因口径的来源）：
// 收盘≥均线不算弱势；收盘<均线、均线不可算、指数不可读都必须算弱势——
// 数据未知时宁可小仓位，绝不按正常风控满仓接刀。
func TestDetectWeakRegime(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/weak.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	days := []string{"20260301", "20260302", "20260303"}
	closes := []model.Fen{300, 200, 100} // 递减序列：末日收盘 100
	for i, d := range days {
		if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
			TsCode: store.MarketIndex, TradeDate: d, Close: closes[i]}); err != nil {
			t.Fatalf("插入指数日线失败: %v", err)
		}
	}

	if weak, note := detectWeakRegime(ctx, st, "20260303", 1); weak || note != "" {
		t.Errorf("收盘=MA1 应判非弱势，实际 weak=%v note=%q", weak, note)
	}
	weak, note := detectWeakRegime(ctx, st, "20260303", 2) // MA2=150 > 收盘100
	if !weak || !strings.Contains(note, "MA2") {
		t.Errorf("收盘低于 MA2 应判弱势且点名窗口，实际 weak=%v note=%q", weak, note)
	}
	weak, note = detectWeakRegime(ctx, st, "20260303", 4) // 只有 3 根：均线不可算
	if !weak || !strings.Contains(note, "不可算") {
		t.Errorf("均线不可算应按弱势降级，实际 weak=%v note=%q", weak, note)
	}
	weak, note = detectWeakRegime(ctx, st, "20260201", 2) // 该日之前无任何指数行
	if !weak || note == "" {
		t.Errorf("指数不可读应按弱势降级，实际 weak=%v note=%q", weak, note)
	}
}

// TestGateOffCapsOf 试探上限解析口径与 MarketGateOf 一致：缺键/坏值回落弱势默认档，
// 越界钳回物理熔断——坏配置最多让仓位更小，不可能让仓位更大。
func TestGateOffCapsOf(t *testing.T) {
	cases := []struct {
		name       string
		cfg        fakeConfig
		wantTotal  float64
		wantSingle float64
	}{
		{"老库缺键", fakeConfig{}, risk.WeakMaxTotalPctDefault, risk.WeakMaxSinglePctDefault},
		{"正常配置", fakeConfig{"screen.gate_off_max_total_pct": "0.30", "screen.gate_off_max_single_pct": "0.15"}, 0.30, 0.15},
		{"越熔断", fakeConfig{"screen.gate_off_max_total_pct": "0.99", "screen.gate_off_max_single_pct": "0.99"},
			risk.CircuitMaxTotalPct, risk.CircuitMaxSinglePct},
		{"负值回落", fakeConfig{"screen.gate_off_max_total_pct": "-1", "screen.gate_off_max_single_pct": "abc"},
			risk.WeakMaxTotalPctDefault, risk.WeakMaxSinglePctDefault},
	}
	for _, c := range cases {
		total, single := gateOffCapsOf(c.cfg)
		if total != c.wantTotal || single != c.wantSingle {
			t.Errorf("%s: 期望 total=%.2f single=%.2f，实际 %.2f/%.2f",
				c.name, c.wantTotal, c.wantSingle, total, single)
		}
	}
}
