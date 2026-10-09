package screener

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// fakeETFRepo ETF 漏斗的最小读取桩：只喂白名单窗口内的日线点。
type fakeETFRepo struct {
	dates []string
	pts   []store.ClosePoint
	err   error // 非 nil 时两个读取都失败，模拟数据链路故障
}

func (f *fakeETFRepo) WindowDates(_ context.Context, _ string, n int) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.dates) < n {
		return f.dates, nil
	}
	return f.dates[len(f.dates)-n:], nil
}

func (f *fakeETFRepo) BarsForCodes(_ context.Context, codes, dates []string) ([]store.ClosePoint, error) {
	if f.err != nil {
		return nil, f.err
	}
	inSet := func(needle string, hay []string) bool {
		for _, h := range hay {
			if h == needle {
				return true
			}
		}
		return false
	}
	var out []store.ClosePoint
	for _, p := range f.pts {
		if inSet(p.TsCode, codes) && inSet(p.TradeDate, dates) {
			out = append(out, p)
		}
	}
	return out, nil
}

// mkDates 生成 n 个升序交易日（末日固定 20261009，与用例里的"当日"对齐）。
func mkDates(n int) []string {
	d := make([]string, 0, n)
	for i := 0; i < n; i++ {
		d = append(d, fmt.Sprintf("%08d", 20260901+i))
	}
	d[n-1] = "20261009"
	return d
}

// mkBars 为某只标的生成若干根日线（收盘价按未复权分计，成交量手）。
func mkBars(code string, dates []string, rawClose float64, volLot float64) []store.ClosePoint {
	var out []store.ClosePoint
	for _, d := range dates {
		out = append(out, store.ClosePoint{TsCode: code, TradeDate: d,
			Close: rawClose, VolLot: volLot, RawClose: rawClose})
	}
	return out
}

func concat(groups ...[]store.ClosePoint) []store.ClosePoint {
	var out []store.ClosePoint
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func TestParseETFWhitelist(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  []ETFInstrument
		count int
	}{
		{"三种写法混用", "588000, 588200:科创芯片ETF, 512480:半导体ETF:半导体设备",
			[]ETFInstrument{{TsCode: "512480.SH", Name: "半导体ETF", Track: "半导体设备"},
				{TsCode: "588000.SH"}, // 裸代码：名称留空，由资格级剔掉
				{TsCode: "588200.SH", Name: "科创芯片ETF", Track: "科创芯片ETF"}}, 3},
		{"分号与换行分隔", "515050:人工智能ETF:AI\n159995.SZ; 159852:科创综指ETF",
			[]ETFInstrument{{TsCode: "159852.SZ", Name: "科创综指ETF", Track: "科创综指ETF"},
				{TsCode: "159995.SZ", Name: "", Track: ""},
				{TsCode: "515050.SH", Name: "人工智能ETF", Track: "AI"}}, 3},
		{"已带后缀不改写", "588000.SH:科创50ETF",
			[]ETFInstrument{{TsCode: "588000.SH", Name: "科创50ETF", Track: "科创50ETF"}}, 1},
		{"空配置", "  ,, ;; ", nil, 0},
	}
	for _, c := range cases {
		got := ParseETFWhitelist(c.raw)
		if len(got) != c.count {
			t.Errorf("%s: 期望 %d 条，实际 %d（%+v）", c.name, c.count, len(got), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: 第 %d 条期望 %+v，实际 %+v", c.name, i, c.want[i], got[i])
			}
		}
	}
}

// TestParseETFWhitelistNoStockPrefix 归一化只可能命中 ETF 代码段：个股（6/0/3 开头）
// 即使被人误写进白名单也不会被加上 .SH/.SZ 伪装成场内基金。
func TestParseETFWhitelistNoStockPrefix(t *testing.T) {
	got := ParseETFWhitelist("600908:无锡银行")
	if len(got) != 1 || got[0].TsCode != "600908" {
		t.Errorf("个股代码不应被补交易所后缀，实际 %+v", got)
	}
}

// TestParseETFWhitelistDedup 同一只标的的两种写法只留一条：
// 重复条目会让同一个 LLM 批次出现两张同一标的的候选，等于把权重双算。
func TestParseETFWhitelistDedup(t *testing.T) {
	got := ParseETFWhitelist("515050.SH:华夏5GETF:通信设备, 515050.sh:另一名称:另一赛道")
	if len(got) != 1 {
		t.Fatalf("重复代码应塌成一条，实际 %+v", got)
	}
	if got[0].Name != "华夏5GETF" {
		t.Errorf("应留第一条的名称，实际 %q", got[0].Name)
	}
	if codes := ETFWhitelistCodes("515050.SH:甲,515050:乙"); len(codes) != 1 || codes[0] != "515050.SH" {
		t.Errorf("代码列应去重且已补后缀，实际 %v", codes)
	}
}

// TestETFWhitelistCodesOrder 代码列与标的清单同源：同步侧与漏斗侧读到同一串代码，
// 否则症状是"日线明明同步了却整天窗口日线不足"，最难归因。
func TestETFWhitelistCodesOrder(t *testing.T) {
	codes := ETFWhitelistCodes(" 515050.sh:华夏中证5G通信主题ETF:通信设备 ;\n159819.SZ:易方达中证人工智能主题ETF ")
	if strings.Join(codes, ",") != "159819.SZ,515050.SH" {
		t.Errorf("codes=%v，期望升序两只且大小写/裸代码都归一", codes)
	}
	if n := len(ETFWhitelistCodes("")); n != 0 {
		t.Errorf("空配置应给出空池，实际 %d 个代码", n)
	}
}

// TestValidateETFWhitelist 装配期严格面（组合根 app.validateETF 复用这一份）。
// 这一族错误的共同形态是"静默 0 候选"，只能在启动那一下拦。
func TestValidateETFWhitelist(t *testing.T) {
	cases := []struct {
		raw     string
		enabled bool
		wantErr bool
		why     string
	}{
		{"515050.SH:华夏中证5G通信主题ETF:通信设备", true, false, "合法组合不该被拒"},
		{"515050:华夏中证5G通信主题ETF:通信设备", true, false, "裸代码归一化后合法（5 段=上交所）"},
		{"159819:易方达人工智能ETF", true, false, "1 段补 .SZ 同样合法"},
		{"515050", false, false, "关闸时缺名称不拦（那是还没启用的配置）"},
		{"", false, false, "关闸时空池不拦"},
		{"", true, true, "开闸却空池"},
		{"515050X.SH:名称:赛道", true, true, "代码形态非法"},
		{"515050XX.SH:名称:赛道", false, true, "代码形态非法在关闸时同样拦"},
		{"515050.SH:名称,515050.SH:另一名称", true, true, "代码重复"},
		{"515050.SH", true, true, "缺名称"},
		{"600908.SH:无锡银行:银行", true, true, "个股代码段与交易所不自洽"},
	}
	for _, tc := range cases {
		err := ValidateETFWhitelist(tc.raw, tc.enabled)
		if tc.wantErr && err == nil {
			t.Errorf("%s：应拒绝启动却放行了（raw=%q）", tc.why, tc.raw)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("%s：不该被拒却报错 %v", tc.why, err)
		}
	}
}

// TestRunETFDropsBadCode 个股误写进白名单时在资格级就被点名剔掉，而不是往后走。
// 它在 daily_bar 里本来就有日线，不加这一级就能一路通过流动性与一手预算，
// 最后披着"ETF"的身份进 LLM 批次 —— 而 ETF 那套证据（无 PE/PB、免印花税、100 份起）对个股是错的。
func TestRunETFDropsBadCode(t *testing.T) {
	dates := mkDates(momentumBars)
	repo := &fakeETFRepo{dates: dates, pts: concat(
		mkBars("588000.SH", dates, 91, 8e6),
		mkBars("600908.SH", dates, 593, 8e6), // 无锡银行：沪市个股段
	)}
	rep, err := RunETF(context.Background(), repo,
		etfOpts(ETFInstrument{TsCode: "588000.SH", Name: "科创50ETF", Track: "科创50"},
			ETFInstrument{TsCode: "600908.SH", Name: "无锡银行", Track: "银行"}),
		etfBudget(), dates[len(dates)-1])
	if err != nil {
		t.Fatalf("RunETF 不应失败: %v", err)
	}
	if len(rep.Candidates) != 1 || rep.Candidates[0].TsCode != "588000.SH" {
		t.Errorf("个股必须被剔，实际候选 %+v", rep.Candidates)
	}
	if n := dropCount(rep.Stages[0].Drops, reasonEtfBadCode); n != 1 {
		t.Errorf("期望按 %q 剔 1 只，实际 %d（drops=%v）", reasonEtfBadCode, n, rep.Stages[0].Drops)
	}
}

// etfBudget 弱势试探期的真实资金口径（总资产 11,826.79、单票上限 10%）。
// perSlot = min(现金/2 槽, 单票上限) = 1182.68 元，与 20261009 实跑一致。
func etfBudget() Budget {
	return Budget{Cash: model.Fen(1123379), Slots: 2, CapFen: model.Fen(118268), MarketOK: true}
}

func etfOpts(pool ...ETFInstrument) ETFOptions {
	return ETFOptions{Enabled: true, Pool: pool,
		MinAvgAmountFen: model.FromFloat(2.0e8), Window: momentumBars}
}

// TestRunETFPool 弱势期两条腿都能过 → 出候选，且**不带任何排名信息**：
// Score/Factors/PoolSize 留零值就是"这只排第几"这个信息不存在，
// 一旦被填上数字，模型会拿它当依据（设计文档 §2 的 IC 实测否掉了排名）。
func TestRunETFPool(t *testing.T) {
	dates := mkDates(momentumBars)
	repo := &fakeETFRepo{dates: dates, pts: append(
		mkBars("588000.SH", dates, 91, 5e6),
		mkBars("512480.SH", dates, 105, 8e6)...)}
	pool := []ETFInstrument{
		{TsCode: "588000.SH", Name: "科创50ETF", Track: "科创50"},
		{TsCode: "512480.SH", Name: "半导体ETF", Track: "半导体设备"},
	}
	rep, err := RunETF(context.Background(), repo, etfOpts(pool...), etfBudget(), "20261009")
	if err != nil {
		t.Fatalf("RunETF 不应失败: %v", err)
	}
	if len(rep.Candidates) != 2 {
		t.Fatalf("期望 2 只候选，实际 %d（summary=%s）", len(rep.Candidates), rep.Summary())
	}
	c := rep.Candidates[0]
	if c.Kind != model.KindETF || !c.IsETF() {
		t.Errorf("候选必须标记为 ETF，实际 Kind=%q", c.Kind)
	}
	if c.Name == "" || c.Industry == "" {
		t.Errorf("名称与跟踪方向必须来自白名单，实际 %+v", c)
	}
	if c.Score != 0 || c.PoolSize != 0 || c.Factors != (model.FactorScore{}) {
		t.Errorf("ETF 不得带排名/打分信息，实际 %+v", c)
	}
	if !strings.Contains(c.Reason, "不排名") {
		t.Errorf("入围依据应写明不排名，实际 %q", c.Reason)
	}
	if c.Close != model.Fen(91) {
		t.Errorf("Close 必须取未复权收盘（与成交成本同口径），实际 %v", c.Close)
	}
	if len(rep.Stages) != 2 || rep.Stages[0].Out != 2 || rep.Stages[1].Out != 2 {
		t.Errorf("两级都应全通过，实际 %+v", rep.Stages)
	}
}

// TestRunETFDrops 四种缺数据/不达标各归一级，且都是计数淘汰而不是报错：
// 某天 fund_daily 没发布 = 当日少一个候选来源，不是整链故障。
func TestRunETFDrops(t *testing.T) {
	dates := mkDates(momentumBars)
	repo := &fakeETFRepo{dates: dates, pts: concat(
		mkBars("512480.SH", dates, 105, 8e6),     // 正常
		mkBars("159995.SZ", dates, 1200, 8e6),    // 一手 1200 元 > 预算线 1182.68 元
		mkBars("159852.SZ", dates, 91, 1e4),      // 日均成交额 9.1 万 < 2 亿
		mkBars("588200.SH", dates[10:], 95, 8e6), // 含当日但只有 10 根 → 窗口不足
		mkBars("515050.SH", dates[:19], 90, 8e6), // 19 根且末日不是当日 → 当日无报价
	)}
	pool := []ETFInstrument{
		{TsCode: "512480.SH", Name: "半导体ETF", Track: "半导体设备"},
		{TsCode: "159995.SZ", Name: "芯片ETF", Track: "芯片"},
		{TsCode: "159852.SZ", Name: "科创综指ETF", Track: "科创综指"},
		{TsCode: "588200.SH", Name: "科创芯片ETF", Track: "科创芯片"},
		{TsCode: "515050.SH", Name: "人工智能ETF", Track: "AI"},
		{TsCode: "588000.SH"}, // 无名称（配置只写了代码）
	}
	rep, err := RunETF(context.Background(), repo, etfOpts(pool...), etfBudget(), "20261009")
	if err != nil {
		t.Fatalf("RunETF 不应失败: %v", err)
	}
	if len(rep.Candidates) != 1 || rep.Candidates[0].TsCode != "512480.SH" {
		t.Fatalf("只应剩正常那只，实际 %+v（summary=%s）", rep.Candidates, rep.Summary())
	}
	poolDrops, liqDrops := rep.Stages[0].Drops, rep.Stages[1].Drops
	// 淘汰原因带具体数值（"窗口 20 根只有 10 根"），断言按前缀匹配。
	for why, want := range map[string]int{
		reasonEtfNoName: 1, reasonEtfNoBar: 1, reasonEtfStale: 1,
	} {
		if got := dropCount(poolDrops, why); got != want {
			t.Errorf("资格级 %q 期望淘汰 %d，实际 %d（%+v）", why, want, got, poolDrops)
		}
	}
	if got := dropCount(liqDrops, reasonEtfIlliquid); got != 1 {
		t.Errorf("流动性级应淘汰成交额不足那只，实际 %d（%+v）", got, liqDrops)
	}
	if got := dropCount(liqDrops, reasonUnaffordable); got != 1 {
		t.Errorf("流动性级应淘汰一手超预算那只，实际 %d（%+v）", got, liqDrops)
	}
	if rep.Stages[0].In != 6 || rep.Stages[0].Out != 3 || rep.Stages[1].Out != 1 {
		t.Errorf("进出计数应为 6→3→1，实际 %+v", rep.Stages)
	}
}

// dropCount 按原因前缀累计淘汰数。
func dropCount(drops map[string]int, prefix string) int {
	n := 0
	for why, c := range drops {
		if strings.HasPrefix(why, prefix) {
			n += c
		}
	}
	return n
}

// TestRunETFCalendarShort 交易日历凑不满窗口必须返回 error（不是静默出 0 候选）：
// 那与"白名单全被淘汰"看起来一模一样，而前者是数据链没跑、后者是可归因的结论。
func TestRunETFCalendarShort(t *testing.T) {
	dates := mkDates(5)
	repo := &fakeETFRepo{dates: dates, pts: mkBars("512480.SH", dates, 105, 8e6)}
	if _, err := RunETF(context.Background(), repo,
		etfOpts(ETFInstrument{TsCode: "512480.SH", Name: "半导体ETF"}), etfBudget(), "20261009"); err == nil {
		t.Fatal("日历不足窗口时应返回 error 交给调用方降级")
	}
}

// TestRunETFRepoError 读取失败原样上抛（由 scheduler 收成降级，不掐链）。
func TestRunETFRepoError(t *testing.T) {
	repo := &fakeETFRepo{err: errors.New("tushare 超时")}
	if _, err := RunETF(context.Background(), repo,
		etfOpts(ETFInstrument{TsCode: "512480.SH", Name: "半导体ETF"}), etfBudget(), "20261009"); err == nil {
		t.Fatal("读取失败应上抛")
	}
}

// TestRunETFEmptyPool 白名单为空 → 一句 note，不报错也不产出。
func TestRunETFEmptyPool(t *testing.T) {
	rep, err := RunETF(context.Background(), &fakeETFRepo{}, etfOpts(), etfBudget(), "20261009")
	if err != nil {
		t.Fatalf("空白名单不是故障: %v", err)
	}
	if !rep.Empty() || len(rep.Notes) == 0 {
		t.Errorf("应返回空报告并带说明，实际 %+v", rep)
	}
}
