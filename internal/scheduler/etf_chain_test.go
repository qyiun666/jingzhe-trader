package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
)

// etfWhitelistForTest 组长批准的主 5 + 备 2（名称按 fund_basic.name 人工复核，不靠代码前缀猜）。
const etfWhitelistForTest = "588000:科创50ETF:科创50,588200:科创芯片ETF:科创芯片," +
	"512480:半导体ETF:半导体设备,515050:人工智能ETF:人工智能," +
	"159819:科创AIETF:科创AI,159995:芯片ETF:芯片,159852:科创综指ETF:科创综指"

func weakOffConfig() fakeConfig {
	return fakeConfig{
		"screen.etf_enabled":             "true",
		"screen.etf_whitelist":           etfWhitelistForTest,
		"screen.etf_min_avg_amount_yi":   "2.0",
		"screen.gate_enabled":            "false",
		"screen.gate_ma_window":          "60",
		"screen.gate_off_max_total_pct":  "0.20",
		"screen.gate_off_max_single_pct": "0.10",
	}
}

// TestEtfShouldRun 启用判据（组长批准的口径逐条落测）：只有"补口开 + 闸门关 + 判弱势"
// 三者同时成立才产 ETF 候选。验收点名的那条是 enabled=true 而 gate=true → 不产。
func TestEtfShouldRun(t *testing.T) {
	opts := screener.ETFOptions{Enabled: true}
	on := store.MarketGate{Enabled: true, Window: 60}
	off := store.MarketGate{Enabled: false, Window: 60}
	weak := screener.Budget{WeakRegime: true}
	strong := screener.Budget{}

	cases := []struct {
		name   string
		opts   screener.ETFOptions
		gate   store.MarketGate
		budget screener.Budget
		want   bool
	}{
		{"批准口径：关闸+弱势+补口开", opts, off, weak, true},
		{"验收：补口开但闸门开着", opts, on, weak, false},
		{"验收：补口开、闸门关、但大盘不弱势", opts, off, strong, false},
		{"补口关（上线默认档）", screener.ETFOptions{}, off, weak, false},
		{"闸门开且不弱势（正常市况）", screener.ETFOptions{}, on, strong, false},
	}
	for _, c := range cases {
		if got := etfShouldRun(c.opts, c.gate, c.budget); got != c.want {
			t.Errorf("%s: 期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}

// TestETFOptionsOf 三键的读取口径：缺键=未启用（与上线前逐位一致）；
// 成交额下限坏值回落默认 2.0 亿而不是"没有下限"；窗口与个股因子窗口同源。
func TestETFOptionsOf(t *testing.T) {
	got := ETFOptionsOf(fakeConfig{})
	if got.Enabled || len(got.Pool) != 0 {
		t.Errorf("老库缺键时应完全未启用，实际 %+v", got)
	}
	if got.Window != screener.BarWindow() {
		t.Errorf("窗口应与个股因子窗口同源（%d），实际 %d", screener.BarWindow(), got.Window)
	}
	if got.MinAvgAmountFen != model.FromFloat(2.0e8) {
		t.Errorf("下限缺键应回落 2.0 亿，实际 %v", got.MinAvgAmountFen)
	}

	o := ETFOptionsOf(weakOffConfig())
	if !o.Enabled || len(o.Pool) != 7 {
		t.Fatalf("白名单 7 只要全部解析出来，实际 enabled=%v pool=%d", o.Enabled, len(o.Pool))
	}
	for _, in := range o.Pool {
		if in.Name == "" || in.Track == "" {
			t.Errorf("白名单条目缺名称/跟踪方向：%+v", in)
		}
	}

	bad := fakeConfig{"screen.etf_enabled": "true", "screen.etf_min_avg_amount_yi": "-1"}
	if ETFOptionsOf(bad).MinAvgAmountFen != model.FromFloat(2.0e8) {
		t.Errorf("负下限应回落默认档（更严），而不是当没有下限")
	}
	if ETFOptionsOf(fakeConfig{"screen.etf_enabled": "yes-please"}).Enabled {
		t.Errorf("开关坏值应按未启用处理")
	}
}

// TestETFUniverseOf 卖出链的代码集**不看 etf_enabled**：停补口只是不再买，
// 存量仓位仍要豁免排名淘汰。只有带名称的条目进 universe。
func TestETFUniverseOf(t *testing.T) {
	if len(ETFUniverseOf(fakeConfig{})) != 0 {
		t.Errorf("空白名单应给空集，让卖出链按个股口径走")
	}
	u := ETFUniverseOf(fakeConfig{"screen.etf_whitelist": "588000:科创50ETF, 512480"})
	if len(u) != 1 || u["588000.SH"] != "科创50ETF" {
		t.Errorf("只应有名称的条目进 universe，实际 %+v", u)
	}
	off := fakeConfig{"screen.etf_enabled": "false", "screen.etf_whitelist": "588000:科创50ETF"}
	if len(ETFUniverseOf(off)) != 1 {
		t.Errorf("补口关闭时 universe 必须照旧（存量仓位的豁免依据）")
	}
}

// seedETFStore 建一个只装交易日历与（可选）ETF 日线的测试库：够 RunETF 两级跑完，
// 不牵扯个股那一侧——本用例要归因的是补口本身。
func seedETFStore(t *testing.T, days int, withBars bool) (*store.Store, string) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/etf.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	ctx := context.Background()
	var dates []string
	for i := 1; i <= days; i++ {
		d := fmt.Sprintf("202609%02d", i)
		dates = append(dates, d)
		if err := st.MarketRepo().UpsertCal(ctx, store.CalRow{CalDate: d, IsOpen: true}); err != nil {
			t.Fatalf("写交易日历失败: %v", err)
		}
	}
	if withBars {
		// 0.91 元/份、500 万手：一手 91 元（在试探预算内），日均成交额 4.55 亿（过流动性线）。
		for _, in := range screener.ParseETFWhitelist(etfWhitelistForTest) {
			for _, d := range dates {
				if err := st.MarketRepo().UpsertBar(ctx, model.Bar{
					TsCode: in.TsCode, TradeDate: d, Close: 91, VolLot: 5e6, RawClose: 91,
				}); err != nil {
					t.Fatalf("写 ETF 日线失败: %v", err)
				}
			}
		}
	}
	return st, dates[len(dates)-1]
}

// weakBudget 弱势试探期的真实资金口径（可用现金 11,233.79、单票上限 1182.68）。
func weakBudget() screener.Budget {
	return screener.Budget{Cash: model.Fen(1123379), Slots: 2, CapFen: model.Fen(118268),
		MarketOK: true, WeakRegime: true, WeakNote: "测试弱势"}
}

var stockCand = []model.Candidate{{TsCode: "600908.SH", Name: "无锡银行"}}

// TestAppendETFCandidatesWeakRegime 弱势期：7 只白名单全部过两级并入个股批次，
// 带 Kind=model.KindETF 与白名单名称；正常产出不该有任何降级。
func TestAppendETFCandidatesWeakRegime(t *testing.T) {
	st, date := seedETFStore(t, screener.BarWindow(), true)
	defer st.Close()
	d := Deps{Store: st, Config: weakOffConfig()}
	rc := observability.NewRunCtx(context.Background(), JobEveningPipeline, date)

	got := appendETFCandidates(context.Background(), rc, d, date, stockCand, weakBudget())
	if len(got) != 1+7 {
		t.Fatalf("弱势期应并入 7 只 ETF 候选，实际 %d 条（降级 %v）", len(got), rc.Degradations())
	}
	for _, c := range got[1:] {
		if !c.IsETF() || c.Name == "" || c.Industry == "" {
			t.Errorf("并入的必须是带名称与跟踪方向的 ETF 候选，实际 %+v", c)
		}
	}
	if rc.Degraded() {
		t.Errorf("补口正常产出不该有降级，实际 %v", rc.Degradations())
	}
}

// TestAppendETFCandidatesGateOpenSilent 开闸日（验收用例）：补口一个字都不读、
// 一条候选都不加，也不留任何降级——ETF 只属于弱势试探期。
func TestAppendETFCandidatesGateOpenSilent(t *testing.T) {
	st, date := seedETFStore(t, screener.BarWindow(), true)
	defer st.Close()
	cfg := weakOffConfig()
	cfg["screen.gate_enabled"] = "true"
	d := Deps{Store: st, Config: cfg}
	rc := observability.NewRunCtx(context.Background(), JobEveningPipeline, date)

	got := appendETFCandidates(context.Background(), rc, d, date, stockCand, weakBudget())
	if len(got) != 1 || got[0].TsCode != "600908.SH" {
		t.Errorf("开闸日不得产 ETF 候选，实际 %+v", got)
	}
	if rc.Degraded() {
		t.Errorf("补口未参与判定不该有降级，实际 %v", rc.Degradations())
	}
}

// TestAppendETFCandidatesNoData 补口开着、判弱势，但 ETF 日线一天都没同步：
// 个股候选照常返回、链不断，同时以 alert:ETF_EMPTY 出声（静默失效与从未启用看起来一样）。
func TestAppendETFCandidatesNoData(t *testing.T) {
	st, date := seedETFStore(t, screener.BarWindow(), false)
	defer st.Close()
	d := Deps{Store: st, Config: weakOffConfig()}
	rc := observability.NewRunCtx(context.Background(), JobEveningPipeline, date)

	got := appendETFCandidates(context.Background(), rc, d, date, stockCand, weakBudget())
	if len(got) != 1 || got[0].TsCode != "600908.SH" {
		t.Fatalf("补口无数据不得影响个股候选，实际 %+v", got)
	}
	if !rc.Degraded() {
		t.Fatal("补口启用却零入围必须出声")
	}
	if traceSubject(t, st, date, "alert:ETF_EMPTY") == "" {
		t.Errorf("应落一行 alert:ETF_EMPTY 供事后按日期捞，实际降级 %v", rc.Degradations())
	}
}

// TestAppendETFCandidatesReadFailNotBreakChain 读取直接失败（日历深度不够窗口）：
// 走 alert:ETF_UNAVAILABLE 而不是把整条 evening_pipeline 掐掉。
func TestAppendETFCandidatesReadFailNotBreakChain(t *testing.T) {
	st, date := seedETFStore(t, 5, false) // 只有 5 个交易日 < 窗口 20 → RunETF 返回 error
	defer st.Close()
	d := Deps{Store: st, Config: weakOffConfig()}
	rc := observability.NewRunCtx(context.Background(), JobEveningPipeline, date)

	got := appendETFCandidates(context.Background(), rc, d, date, stockCand, weakBudget())
	if len(got) != 1 {
		t.Fatalf("读取失败时个股候选必须原样返回，实际 %+v", got)
	}
	detail := traceSubject(t, st, date, "alert:ETF_UNAVAILABLE")
	if detail == "" {
		t.Fatalf("读取失败要落 alert:ETF_UNAVAILABLE，实际降级 %v", rc.Degradations())
	}
	if !strings.Contains(detail, "窗口") || !strings.Contains(detail, "ETF 补口") {
		t.Errorf("轨迹行应能自解释（补口 + 窗口不足），实际 %q", detail)
	}
}

// traceSubject 读回当日某一行 alert 轨迹的 detail（不存在返回空串）。
func traceSubject(t *testing.T, st *store.Store, date, subject string) string {
	t.Helper()
	rows, err := st.TraceRepo().List(context.Background(), date)
	if err != nil {
		t.Fatalf("读轨迹失败: %v", err)
	}
	for _, r := range rows {
		if r.Subject == subject {
			if r.Outcome != model.TracePartial {
				t.Errorf("%s 应为 partial（降级不是故障），实际 %q", subject, r.Outcome)
			}
			return r.Detail
		}
	}
	return ""
}
