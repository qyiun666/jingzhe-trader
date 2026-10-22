package attribution

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

var testDates = []string{
	"20261009", "20261012", "20261013", "20261014", "20261015", "20261016",
	"20261019", "20261020", "20261021", "20261022", "20261023",
}

// newFixture 造一个最小归因库：可选的 3 条决策（买/跳过/失败）或仅证据行、
// 可选候选快照、三只股票与沪深300 在 10-09/10-16/10-23 的日线。
func newFixture(t *testing.T, withSnapshot, withDecisions bool) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/attr.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	for _, d := range testDates {
		if err := st.MarketRepo().UpsertCal(ctx, store.CalRow{CalDate: d, IsOpen: true}); err != nil {
			t.Fatalf("写日历 %s 失败: %v", d, err)
		}
	}
	for _, s := range []model.StockBasic{
		{TsCode: "600001.SH", Name: "甲", Industry: "银行", ListStatus: "L"},
		{TsCode: "600002.SH", Name: "乙", Industry: "半导体", ListStatus: "L"},
		{TsCode: "600003.SH", Name: "丙", Industry: "医药", ListStatus: "L"},
	} {
		if err := st.MarketRepo().UpsertStockBasic(ctx, s); err != nil {
			t.Fatalf("写股票基础失败: %v", err)
		}
	}

	bars := []model.Bar{
		{TsCode: "600001.SH", TradeDate: "20261009", Close: 1000, VolLot: 100, RawClose: 1000},
		{TsCode: "600001.SH", TradeDate: "20261016", Close: 1100, VolLot: 100, RawClose: 1100},
		{TsCode: "600001.SH", TradeDate: "20261023", Close: 1200, VolLot: 100, RawClose: 1200},
		{TsCode: "600002.SH", TradeDate: "20261009", Close: 2000, VolLot: 100, RawClose: 2000},
		{TsCode: "600002.SH", TradeDate: "20261016", Close: 1900, VolLot: 100, RawClose: 1900},
		{TsCode: "600002.SH", TradeDate: "20261023", Close: 1800, VolLot: 100, RawClose: 1800},
		{TsCode: "600003.SH", TradeDate: "20261009", Close: 3000, VolLot: 100, RawClose: 3000},
		{TsCode: "600003.SH", TradeDate: "20261016", Close: 3150, VolLot: 100, RawClose: 3150},
		{TsCode: "600003.SH", TradeDate: "20261023", Close: 3300, VolLot: 100, RawClose: 3300},
		{TsCode: IndexCode, TradeDate: "20261009", Close: 4000, VolLot: 0, RawClose: 0},
		{TsCode: IndexCode, TradeDate: "20261016", Close: 4040, VolLot: 0, RawClose: 0},
		{TsCode: IndexCode, TradeDate: "20261023", Close: 4080, VolLot: 0, RawClose: 0},
	}
	for _, b := range bars {
		if err := st.MarketRepo().UpsertBar(ctx, b); err != nil {
			t.Fatalf("写日线 %s/%s 失败: %v", b.TsCode, b.TradeDate, err)
		}
	}

	if withDecisions {
		for _, c := range []model.LLMCall{
			{TradeDate: "20261009", TsCode: "600001.SH", PromptKey: "decision",
				Verdict: "buy", Confidence: 0.80, WeightPct: 0.30, Rationale: "形态好",
				Status: model.TraceOK, CreatedAt: now},
			{TradeDate: "20261009", TsCode: "600002.SH", PromptKey: "decision",
				Verdict: "skip", Confidence: 0.60, Rationale: "估值高",
				Status: model.TraceOK, CreatedAt: now},
			{TradeDate: "20261009", TsCode: "600003.SH", PromptKey: "decision",
				Status: model.TraceFail, Error: "评审未问出结果", CreatedAt: now},
		} {
			if err := st.LLMRepo().SaveCall(ctx, c); err != nil {
				t.Fatalf("写决策留痕失败: %v", err)
			}
		}
	} else {
		// 整批证据失败、连 decision 行都没有的日期：只剩证据行可还原候选。
		for _, code := range []string{"600001.SH", "600002.SH"} {
			for _, key := range []string{"tech", "value", "sector"} {
				if err := st.LLMRepo().SaveCall(ctx, model.LLMCall{
					TradeDate: "20261009", TsCode: code, PromptKey: key,
					Verdict: "neutral", Status: model.TraceOK, CreatedAt: now,
				}); err != nil {
					t.Fatalf("写证据留痕失败: %v", err)
				}
			}
		}
	}

	if withSnapshot {
		snap := model.ScreenSnapshot{
			TradeDate: "20261009", TotalAssetFen: 1_000_000, SlotFen: 500_000,
			Candidates: []model.CandidateSnapshot{
				{TsCode: "600001.SH", Name: "甲", Industry: "银行", Rank: 1, PoolSize: 20,
					Score: 80, Momentum: 90, Value: 70, LowVol: 60, Liquidity: 50, CloseFen: 1000},
				{TsCode: "600002.SH", Name: "乙", Rank: 2, PoolSize: 20,
					Score: 60, Momentum: 50, Value: 40, LowVol: 70, Liquidity: 30, CloseFen: 2000},
				{TsCode: "600003.SH", Name: "丙", Industry: "医药", Rank: 3, PoolSize: 20,
					Score: 50, CloseFen: 3000},
			},
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			t.Fatalf("序列化快照失败: %v", err)
		}
		if err := st.TraceRepo().Write(ctx, model.RunTrace{
			TradeDate: "20261009", Subject: model.TraceScreenTopN(),
			Outcome: model.TraceOK, Detail: string(raw), At: now,
		}); err != nil {
			t.Fatalf("写候选快照失败: %v", err)
		}
	}
	return st
}

func bucketOf(t *testing.T, buckets []Bucket, key string) Bucket {
	t.Helper()
	b := findBucket(buckets, key)
	if b == nil {
		t.Fatalf("缺少分组 %q，实际 %+v", key, buckets)
	}
	return *b
}

func near(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-5 {
		t.Errorf("%s=%.6f，期望 %.6f", name, got, want)
	}
}

// TestComputeAttribution 端到端：买入/跳过/失败三层，5 日与 10 日收益、
// 相对沪深300 超额、成本扣除、候选快照里的买入额与因子都要正确落到 Report。
func TestComputeAttribution(t *testing.T) {
	st := newFixture(t, true, true)
	rep, err := Compute(context.Background(), st, Options{From: "20261009", To: "20261009"})
	if err != nil {
		t.Fatalf("Compute 失败: %v", err)
	}
	if rep.Decisions != 3 || rep.Snapshots != 1 {
		t.Fatalf("决策=%d 快照=%d，期望 3/1", rep.Decisions, rep.Snapshots)
	}
	if len(rep.Overall) != 3 {
		t.Fatalf("默认应有 5/10/20 三个窗口，实际 %d", len(rep.Overall))
	}

	// 5 日：n=3，净收益按各自买入额扣双边成本。
	h5 := rep.Overall[0]
	if h5.Horizon != 5 || h5.N != 3 || h5.IndexN != 3 {
		t.Fatalf("5日汇总=%+v，期望 n=3 indexN=3", h5)
	}
	want5 := (0.095385 + -0.052917 + 0.046880) / 3
	near(t, "5日平均净收益", h5.AvgNet, want5)
	near(t, "5日平均超额", h5.AvgExcess, want5-0.01)

	// 10 日：10-23 收盘可用，20 日：目标日 11-06 尚未到期。
	h10 := rep.Overall[1]
	if h10.Horizon != 10 || h10.N != 3 {
		t.Fatalf("10日汇总=%+v，期望 n=3", h10)
	}
	h20 := rep.Overall[2]
	if h20.Horizon != 20 || h20.N != 0 || h20.Pending != 3 {
		t.Fatalf("20日应全部待到期，实际 %+v", h20)
	}

	buy := bucketOf(t, rep.ByVerdict, VerdictBuy)
	near(t, "买入 5 日净收益", buy.Results[0].AvgNet, 0.095385)
	if buy.Results[0].N != 1 {
		t.Errorf("买入桶 n=%d，期望 1", buy.Results[0].N)
	}
	skip := bucketOf(t, rep.ByVerdict, VerdictSkip)
	near(t, "跳过 5 日净收益", skip.Results[0].AvgNet, -0.052917)
	failed := bucketOf(t, rep.ByVerdict, VerdictFailed)
	near(t, "失败 5 日净收益", failed.Results[0].AvgNet, 0.046880)

	// 置信度分层：0.80 → ≥0.80，0.60 → 0.60-0.70，失败行 → 未给置信。
	if b := bucketOf(t, rep.ByConfidence, "≥0.80"); b.Rows != 1 {
		t.Errorf("≥0.80 桶应有 1 条，实际 %d", b.Rows)
	}
	if b := bucketOf(t, rep.ByConfidence, "0.60-0.70"); b.Rows != 1 {
		t.Errorf("0.60-0.70 桶应有 1 条，实际 %d", b.Rows)
	}
	if b := bucketOf(t, rep.ByConfidence, "未给置信"); b.Rows != 1 {
		t.Errorf("未给置信桶应有 1 条，实际 %d", b.Rows)
	}

	// 快照里的买入额：买入行按 weight×总资产（0.3×10000=3000 元，未超单票上限），
	// 跳过/失败行按单票上限 5000 元。
	wantNotional := map[string]model.Fen{
		"600001.SH": model.FromFloat(3000),
		"600002.SH": model.FromFloat(5000),
		"600003.SH": model.FromFloat(5000),
	}
	for _, r := range rep.Rows {
		if r.NotionalFen != wantNotional[r.TsCode] {
			t.Errorf("%s 买入额=%s，期望 %s", r.TsCode, r.NotionalFen, wantNotional[r.TsCode])
		}
	}
	for _, r := range rep.Rows {
		if r.TsCode == "600001.SH" {
			if r.Score != 80 || r.Factors.Momentum != 90 {
				t.Errorf("候选快照的 score/factors 未带入: %+v", r)
			}
		}
		if r.TsCode == "600002.SH" && r.Industry != "半导体" {
			t.Errorf("快照缺行业时应从 stock_basic 补齐，实际 %q", r.Industry)
		}
	}
}

// TestComputeWithoutSnapshot 旧留痕没有 screen:topn：不得因此失败；
// 买入额回落到 5000 元参考额，名称/行业从 stock_basic 补，Notes 明确说明。
func TestComputeWithoutSnapshot(t *testing.T) {
	st := newFixture(t, false, true)
	rep, err := Compute(context.Background(), st, Options{From: "20261009", To: "20261009"})
	if err != nil {
		t.Fatalf("Compute 失败: %v", err)
	}
	if rep.Snapshots != 0 {
		t.Errorf("Snapshots=%d，期望 0", rep.Snapshots)
	}
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "没有候选快照") {
		t.Errorf("Notes 应说明快照缺失: %v", rep.Notes)
	}
	for _, r := range rep.Rows {
		if r.NotionalFen != 5000*100 {
			t.Errorf("%s 无快照时买入额=%s，期望 5000 元", r.TsCode, r.NotionalFen)
		}
		if r.Industry == "" {
			t.Errorf("%s 行业应从 stock_basic 补齐", r.TsCode)
		}
	}
}

// TestComputeReconstructsFailedCohort 整批证据失败（无 decision 行）的日期：
// 从证据行还原候选，按"评审失败"计入前瞻收益；不能因为缺 decision 就把当天从归因里抹掉。
func TestComputeReconstructsFailedCohort(t *testing.T) {
	st := newFixture(t, false, false)
	rep, err := Compute(context.Background(), st, Options{From: "20261009", To: "20261009"})
	if err != nil {
		t.Fatalf("Compute 失败: %v", err)
	}
	if rep.Decisions != 0 || rep.Reconstructed != 2 {
		t.Fatalf("决策=%d 还原=%d，期望 0/2", rep.Decisions, rep.Reconstructed)
	}
	if len(rep.Rows) != 2 {
		t.Fatalf("应还原 2 条候选，实际 %d", len(rep.Rows))
	}
	for _, r := range rep.Rows {
		if r.Verdict != VerdictFailed {
			t.Errorf("还原候选 %s 应归入评审失败，实际 %q", r.TsCode, r.Verdict)
		}
		if r.Industry == "" {
			t.Errorf("还原候选 %s 缺行业（应取自 stock_basic）", r.TsCode)
		}
	}
	failed := bucketOf(t, rep.ByVerdict, VerdictFailed)
	if failed.Rows != 2 || failed.Results[0].N != 2 {
		t.Errorf("失败桶=%+v，期望 2 条且 5 日 n=2", failed)
	}
	near(t, "还原 5 日平均净收益", failed.Results[0].AvgNet, (0.096781-0.052917)/2)
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "证据行还原") {
		t.Errorf("Notes 应说明还原来源: %v", rep.Notes)
	}
	line, err := SummaryLine(context.Background(), st, "20261023", 20, 5)
	if err != nil {
		t.Fatalf("SummaryLine 失败: %v", err)
	}
	if !strings.Contains(line, "评审失败 2 只") || strings.Contains(line, "无 LLM 决策留痕") {
		t.Errorf("摘要应显示还原后的失败候选: %s", line)
	}
}

// TestSummaryLine M5 摘要：买入/跳过/失败三段都要出现，5 日与 20 日
// （未到期）各自的措辞正确。
func TestSummaryLine(t *testing.T) {
	st := newFixture(t, true, true)
	line, err := SummaryLine(context.Background(), st, "20261023", 20, 5)
	if err != nil {
		t.Fatalf("SummaryLine 失败: %v", err)
	}
	for _, want := range []string{"近 11 个交易日", "买入 1 只", "+9.54%", "跳过 1 只", "评审失败 1 只"} {
		if !strings.Contains(line, want) {
			t.Errorf("5 日摘要缺少 %q: %s", want, line)
		}
	}
	line20, err := SummaryLine(context.Background(), st, "20261023", 20, 20)
	if err != nil {
		t.Fatalf("20 日摘要失败: %v", err)
	}
	if n := strings.Count(line20, "待到期 1"); n != 3 {
		t.Errorf("20 日摘要应三段都说明待到期，实际 %d 段: %s", n, line20)
	}
}

// TestHorizonStatus 目标日无日线的归类：今天及未来算待到期，过去缺线才算缺数据。
// 这条直接决定 10-09 cohort 的 10 日结果在 10-23 盘前显示"待到期"而不是"缺数据"。
func TestHorizonStatus(t *testing.T) {
	cases := []struct {
		target, today string
		wantPending   bool
		wantMissing   bool
	}{
		{"20261023", "20261023", true, false}, // 今天还没收盘
		{"20261106", "20261023", true, false}, // 未来
		{"20261022", "20261023", false, true}, // 过去仍缺线
	}
	for _, c := range cases {
		p, m := horizonStatus(c.target, c.today, false)
		if p != c.wantPending || m != c.wantMissing {
			t.Errorf("target=%s today=%s → pending=%v missing=%v，期望 %v/%v",
				c.target, c.today, p, m, c.wantPending, c.wantMissing)
		}
		if p, m := horizonStatus(c.target, c.today, true); p || m {
			t.Errorf("有日线时不应待到期或缺数据")
		}
	}
}

// TestNetReturnCosts 净收益公式的边界：买入额不同，最低佣金对小单的拖累不同。
func TestNetReturnCosts(t *testing.T) {
	// 3000 元买入、+10%：双边成本约 13.36 元，净收益约 9.54%。
	near(t, "3000元+10%", netReturn(1000, 1100, 300000), 0.095385)
	// 5000 元买入、-5%：净收益约 -5.29%。
	near(t, "5000元-5%", netReturn(2000, 1900, 500000), -0.052917)
	if got := netReturn(0, 1100, 500000); !math.IsNaN(got) {
		t.Errorf("非法买入价应返回 NaN，实际 %v", got)
	}
	if got := netReturn(1000, 1100, 0); !math.IsNaN(got) {
		t.Errorf("非法买入额应返回 NaN，实际 %v", got)
	}
}
