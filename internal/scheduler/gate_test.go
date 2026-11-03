package scheduler

import (
	"context"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/signal"
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

	if weak, note := detectWeakRegime(ctx, st, "20260303", store.MarketGate{Window: 1}); weak || note != "" {
		t.Errorf("收盘=MA1 应判非弱势，实际 weak=%v note=%q", weak, note)
	}
	weak, note := detectWeakRegime(ctx, st, "20260303", store.MarketGate{Window: 2}) // MA2=150 > 收盘100
	if !weak || !strings.Contains(note, "MA2") {
		t.Errorf("收盘低于 MA2 应判弱势且点名窗口，实际 weak=%v note=%q", weak, note)
	}
	weak, note = detectWeakRegime(ctx, st, "20260303", store.MarketGate{Window: 4}) // 只有 3 根：均线不可算
	if !weak || !strings.Contains(note, "不可算") {
		t.Errorf("均线不可算应按弱势降级，实际 weak=%v note=%q", weak, note)
	}
	weak, note = detectWeakRegime(ctx, st, "20260201", store.MarketGate{Window: 2}) // 该日之前无任何指数行
	if !weak || note == "" {
		t.Errorf("指数不可读应按弱势降级，实际 weak=%v note=%q", weak, note)
	}
	// ConfirmDays 缺省（=0，手工构造的 gate）走历史口径：站上均线即转强，不等确认。
	if weak, note = detectWeakRegime(ctx, st, "20260303", store.MarketGate{Window: 1}); weak {
		t.Errorf("未显式配置防抖天数时应保持历史口径，实际 weak=%v note=%q", weak, note)
	}
}

// TestDetectWeakRegimeRequiresConsecutiveConfirmation 定参④ 的调度侧接线：
// 收盘站上均线**不等于**转强，必须连续 ConfirmDays 日都站上各自当日均线，
// 才把仓位从"弱势试探档"放回常规档。单日反抽就放开（旧口径）等于把单票上限
// 从 10% 一次放回 40%，而本轮全部亏损都来自"弱势里硬做小单"这一侧。
//
// 夹具用同一条数据分别过两种口径，这样"防抖真的在拦"是可证的而不是注释里的声称：
// 收盘 10,10,8,10,12（MA3 逐日 9.33 / 9.33 / 10）：末日 12 站上均线，
// 但倒数第 3 日 8 在其日均线下方 → 连续确认中断。
func TestDetectWeakRegimeRequiresConsecutiveConfirmation(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/confirm.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	days := []string{"20260301", "20260302", "20260303", "20260304", "20260305"}
	closes := []model.Fen{1000, 1000, 800, 1000, 1200}
	for i, d := range days {
		if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
			TsCode: store.MarketIndex, TradeDate: d, Close: closes[i]}); err != nil {
			t.Fatalf("插入指数日线失败: %v", err)
		}
	}

	// 旧口径（ConfirmDays=1）：末日 1200 ≥ MA3=1000 → 判非弱势。
	if weak, note := detectWeakRegime(ctx, st, "20260305",
		store.MarketGate{Window: 3, ConfirmDays: 1}); weak {
		t.Errorf("回退档应保持历史口径（站上均线即转强），实际 weak=%v note=%q", weak, note)
	}

	// 加防抖（ConfirmDays=3）：同一条数据必须仍判弱势，且 note 点名"未获连续 3 日确认"。
	weak, note := detectWeakRegime(ctx, st, "20260305", store.MarketGate{Window: 3, ConfirmDays: 3})
	if !weak {
		t.Fatalf("连续确认被中途一天打断，绝不能判转强（那等于把单票上限放回 40%%），实际 weak=%v note=%q", weak, note)
	}
	if !strings.Contains(note, "未获连续 3 日确认") || !strings.Contains(note, "收盘已站上 MA3") {
		t.Errorf("note 要同时写明「已经站上」与「确认未过」两件事供归因，实际 %q", note)
	}

	// 补齐连续三日：最后三根收盘 1200,1201,1202（各自 MA3 都 ≤ 收盘）→ 转强放开。
	for i, d := range []string{"20260303", "20260304", "20260305"} {
		if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
			TsCode: store.MarketIndex, TradeDate: d, Close: model.Fen(1200 + i)}); err != nil {
			t.Fatalf("改写指数日线失败: %v", err)
		}
	}
	if weak, note := detectWeakRegime(ctx, st, "20260305",
		store.MarketGate{Window: 3, ConfirmDays: 3}); weak {
		t.Errorf("连续 3 日站上均线后应放开常规档，实际 weak=%v note=%q", weak, note)
	}

	// 确认序列深度不足（window+confirmDays-1 根读不满）必须读成"未转强"，
	// 不能读成"没有反例所以放行"：这里窗口 2 + 确认 5 需要 6 根，库里只有 5 根，
	// 而当日收盘确实站在 MA2 上方——不写清这一条，测试很容易以为拦的是方向。
	if weak, note := detectWeakRegime(ctx, st, "20260305",
		store.MarketGate{Window: 2, ConfirmDays: 5}); !weak || !strings.Contains(note, "按未转强处理") {
		t.Errorf("确认序列长度不足应按弱势降级并点名数据不足，实际 weak=%v note=%q", weak, note)
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

// TestWeakExitPolicyOf 弱势减仓配置的解析口径：老库缺键 → 启用 + 3% + 50%；
// 坏值/越界回落到默认档（保护性规则宁可保守，不因配置写坏把减仓变成清仓）。
func TestWeakExitPolicyOf(t *testing.T) {
	cases := []struct {
		name string
		cfg  fakeConfig
		want signal.WeakExitPolicy
	}{
		{"老库缺键", fakeConfig{}, signal.WeakExitPolicy{Enabled: true, DropPct: 0.03, ReducePct: 0.5}},
		{"自定义", fakeConfig{
			"signal.weak_exit_enabled": "false", "signal.weak_exit_drop_pct": "0.05",
			"signal.weak_exit_reduce_pct": "0.3",
		}, signal.WeakExitPolicy{Enabled: false, DropPct: 0.05, ReducePct: 0.3}},
		{"坏值回落", fakeConfig{
			"signal.weak_exit_enabled": "abc", "signal.weak_exit_drop_pct": "0.5",
			"signal.weak_exit_reduce_pct": "1.0",
		}, signal.WeakExitPolicy{Enabled: true, DropPct: 0.03, ReducePct: 0.5}},
	}
	for _, c := range cases {
		got := WeakExitPolicyOf(c.cfg)
		if got != c.want {
			t.Errorf("%s: 期望 %+v，实际 %+v", c.name, c.want, got)
		}
	}
}
