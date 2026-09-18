package screener

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// 关闸影子漏斗的回归。这些用例覆盖的是 Screener.Run 这条此前没有任何测试的链路
// （原先只有 itest 用真库真数据跑它），而关闸分支刚从上次的"整级清零后立即 return"
// 改成了"跑完漏斗、结果落影子、候选保持为空"。

const (
	shadowCodes = 12 // 板块排名要求可算动量的成员数下限（minSectorDataMembers=10），少于此数进不了 hot 集合
	shadowDays  = 20 // momentumBars
	shadowDate  = "20260720"
)

func shadowCfg() FilterConfig {
	return FilterConfig{TopN: 20, MinCircMvW: 500000, MinTurnoverRate: 1.0, PriceLow: 2.0,
		PETtmMax: 80, PBMax: 10, MinListDays: 60, SectorTopK: 1, MinSectorMembers: 10}
}

// seedShadow 造一个够跑完整条漏斗的最小库：20 个交易日日历 + 12 只同行业在市票的全窗口日线。
func seedShadow(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/shadow.db")
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	for d := 1; d <= shadowDays; d++ {
		date := fmt.Sprintf("202607%02d", d)
		if err := st.MarketRepo().UpsertCal(ctx, store.CalRow{CalDate: date, IsOpen: true}); err != nil {
			t.Fatalf("写日历 %s: %v", date, err)
		}
		for i := 0; i < shadowCodes; i++ {
			code := fmt.Sprintf("60%04d.SH", i)
			// 收盘价逐日抬升且各票步长不同，保证因子有区分度、TopN 排名稳定。
			close := model.Fen(1000 + 10*i + d*(1+i%3))
			if err := st.MarketRepo().UpsertBar(ctx, model.Bar{TsCode: code, TradeDate: date,
				Close: close, VolLot: 1000, RawClose: close}); err != nil {
				t.Fatalf("写日线 %s/%s: %v", code, date, err)
			}
		}
	}
	vals := make([]model.Valuation, 0, shadowCodes)
	for i := 0; i < shadowCodes; i++ {
		code := fmt.Sprintf("60%04d.SH", i)
		// 静态列与估值列各有各的写入接口（UpsertStockBasic 只碰静态四列，
		// 把估值塞进去会被静默忽略，板块加权就因 CircMvW=0 算不出而筛空）。
		if err := st.MarketRepo().UpsertStockBasic(ctx, model.StockBasic{
			TsCode: code, Name: fmt.Sprintf("测试%d", i),
			Industry: "银行", ListDate: "20260101", ListStatus: "L",
		}); err != nil {
			t.Fatalf("写股票基础 %s: %v", code, err)
		}
		vals = append(vals, model.Valuation{TsCode: code, TurnoverRate: 1.5, PETtm: 20, PB: 2, CircMvW: 600000})
	}
	if err := st.MarketRepo().SaveValuation(ctx, shadowDate, vals); err != nil {
		t.Fatalf("写估值截面: %v", err)
	}
	return st
}

func shadowBudget(marketOK bool) Budget {
	return Budget{Cash: model.FromFloat(100000), Slots: 1, MarketOK: marketOK}
}

func stageSlugs(rep *Report) string {
	parts := make([]string, 0, len(rep.Stages))
	for _, st := range rep.Stages {
		parts = append(parts, st.Slug)
	}
	return strings.Join(parts, ",")
}

// TestClosedGateRunsFullFunnelAsShadow 关闸时漏斗必须跑完每一级并把结果落进 Shadow，
// 同时 Candidates 保持 nil —— "关闸不出单"这条不变量靠候选为空成立，而不是靠中途 return。
// 影子清单与同一天开闸时的候选逐只一致，证明它是同一条漏斗的 dry-run，不是另一套实现。
func TestClosedGateRunsFullFunnelAsShadow(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	s := New(st, shadowCfg(), ModeReversal)

	open, err := s.Run(ctx, shadowDate, shadowBudget(true))
	if err != nil {
		t.Fatalf("开闸跑漏斗失败: %v", err)
	}
	if open.RegimeClosed || open.Empty || len(open.Candidates) != shadowCodes {
		t.Fatalf("开闸基线不对：closed=%t empty=%t 候选=%d", open.RegimeClosed, open.Empty, len(open.Candidates))
	}
	// 关闸当日不该有告警行（这一句必须在跑关闸案例之前断言：同一 (日期,subject) 会互相覆盖）。
	if _, ok, err := st.TraceRepo().LatestBySubject(ctx, model.TraceAlert(AlertCodeScreenEmpty), shadowDate); err != nil || ok {
		t.Fatalf("开闸且候选非空时不应落 SCREEN_EMPTY，实际存在=%t err=%v", ok, err)
	}

	closed, err := s.Run(ctx, shadowDate, shadowBudget(false))
	if err != nil {
		t.Fatalf("关闸跑漏斗失败: %v", err)
	}
	if !closed.RegimeClosed || !closed.Empty {
		t.Errorf("关闸旗标与 Empty 应同时成立，实际 closed=%t empty=%t", closed.RegimeClosed, closed.Empty)
	}
	if closed.Candidates != nil {
		t.Errorf("关闸时候选必须为 nil（非空就会进决策链下真单），实际 %d 只", len(closed.Candidates))
	}
	for _, slug := range []string{"sector", "budget", "liq", "val", "topn"} {
		if !strings.Contains(stageSlugs(closed), slug) {
			t.Errorf("关闸时漏斗应跑完 %q 级，实际各级：%s", slug, stageSlugs(closed))
		}
	}
	if len(closed.Shadow) != len(open.Candidates) {
		t.Fatalf("影子清单应等于开闸候选（%d 只），实际 %d 只", len(open.Candidates), len(closed.Shadow))
	}
	for i := range closed.Shadow {
		if closed.Shadow[i].TsCode != open.Candidates[i].TsCode {
			t.Errorf("第 %d 名不一致：影子 %s vs 候选 %s", i, closed.Shadow[i].TsCode, open.Candidates[i].TsCode)
		}
	}
	if closed.ScoredTotal == 0 {
		t.Error("关闸时打分样本不应为 0（原实现短路后报的是 0，漏斗是否健康看不出来）")
	}
}

// TestClosedGateAlertIsNotAFault 关闸的 SCREEN_EMPTY 是 partial 且不再喊"请人工介入"，
// 正文带影子清单；只有漏斗某级真把池子筛光才是 fail 并要人介入。
func TestClosedGateAlertIsNotAFault(t *testing.T) {
	ctx := context.Background()
	st := seedShadow(t)
	s := New(st, shadowCfg(), ModeReversal)
	if _, err := s.Run(ctx, shadowDate, shadowBudget(false)); err != nil {
		t.Fatalf("关闸跑漏斗失败: %v", err)
	}
	tr, ok, err := st.TraceRepo().LatestBySubject(ctx, model.TraceAlert(AlertCodeScreenEmpty), shadowDate)
	if err != nil || !ok {
		t.Fatalf("关闸应落一条 SCREEN_EMPTY 轨迹，实际存在=%t err=%v", ok, err)
	}
	if tr.Outcome != model.TracePartial {
		t.Errorf("关闸是规则的正常输出，定级应为 %q，实际 %q", model.TracePartial, tr.Outcome)
	}
	if strings.Contains(tr.Detail, "请人工介入") {
		t.Errorf("关闸不该喊人工介入，实际: %s", tr.Detail)
	}
	if !strings.Contains(tr.Detail, "若开闸") {
		t.Errorf("正文应给出影子清单，实际: %s", tr.Detail)
	}

	// 反例：开闸但流动性门槛高到把池子筛光 —— 这才是要人看的故障。
	tight := shadowCfg()
	tight.MinCircMvW = 1e12
	if _, err := New(st, tight, ModeReversal).Run(ctx, shadowDate, shadowBudget(true)); err != nil {
		t.Fatalf("开闸跑漏斗失败: %v", err)
	}
	tr2, ok, err := st.TraceRepo().LatestBySubject(ctx, model.TraceAlert(AlertCodeScreenEmpty), shadowDate)
	if err != nil || !ok {
		t.Fatalf("筛光池子应落 SCREEN_EMPTY，实际存在=%t err=%v", ok, err)
	}
	if tr2.Outcome != model.TraceFail || !strings.Contains(tr2.Detail, "请人工介入") {
		t.Errorf("非关闸筛光应为 fail 且要人介入，实际 %q / %s", tr2.Outcome, tr2.Detail)
	}
	if strings.Contains(tr2.Detail, "若开闸") {
		t.Errorf("开闸日的告警不该出现影子措辞，实际: %s", tr2.Detail)
	}
}

// TestShadowBriefEmptyPool 关闸且下游同样一只选不出时，影子摘要必须明说"需在开闸前排查"，
// 而不是留一句空话让人以为一切正常。
func TestShadowBriefEmptyPool(t *testing.T) {
	if got := (&Report{RegimeClosed: true}).ShadowBrief(); !strings.Contains(got, "需在开闸前排查") {
		t.Errorf("影子清单为空时应给出待排查提示，实际: %q", got)
	}
	if got := (&Report{}).ShadowBrief(); got != "" {
		t.Errorf("非关闸不该有影子摘要，实际: %q", got)
	}
}
