package signal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// etfHolding 一只成本 1.00 元、现价 1.00 元的 ETF（不触发任何价格型规则）。
func etfHolding(isETF bool) HoldingCtx {
	return HoldingCtx{
		Pos:       model.Position{TsCode: "588000.SH", TotalQty: model.Qty(1100), CostPrice: 100, HighPrice: 100},
		LastClose: 100, LastDate: "20261009", InTopN: false, IsETF: isETF,
	}
}

// TestEvalSellETFExemptFromRankOut 排名淘汰对 ETF 不成立（组长批准的豁免）：
// ETF 候选只在弱势试探期产出且不排名，按个股口径"当日不在 TopN 就卖"会在
// 恢复开闸当晚无条件清掉 ETF 仓位——那是规则输入不存在，不是标的变差。
//
// 同时必须确认豁免没有把价格型风控一起关掉：同一只 ETF 跌破止损线照样出信号。
func TestEvalSellETFExemptFromRankOut(t *testing.T) {
	p := risk.DefaultParams(model.Fen(1182679))

	if sig := EvalSell("20261009", etfHolding(true), p, 0, 0); sig != nil {
		t.Errorf("ETF 持仓不该被排名淘汰卖出，实际 %+v", sig)
	}
	sig := EvalSell("20261009", etfHolding(false), p, 0, 0)
	if sig == nil || sig.Rule != RuleRankOut {
		t.Errorf("个股持仓仍须排名淘汰，实际 %+v", sig)
	}

	weak := etfHolding(true)
	weak.LastClose = 90 // 成本 1.00 → 现价 0.90，跌幅 10% > 止损线 8%
	stop := EvalSell("20261009", weak, p, 0, 0)
	if stop == nil || stop.Rule != RuleStopLoss {
		t.Errorf("ETF 的止损必须照旧，实际 %+v", stop)
	}
}

// TestSellNameFallback ETF 名称回落：stock_basic 里没有 ETF 行时，
// 用白名单里人工复核过的名称，而不是让取名称这件事掐断卖出链。
// 个股缺行必须照旧报错——那说明数据链没跑，是需要出声的故障。
func TestSellNameFallback(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/name.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	s := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: false, Window: 60},
		ETFUniverse{"588000.SH": "科创50ETF"})

	if nm, err := s.SellName(ctx, "588000.SH"); err != nil || nm != "科创50ETF" {
		t.Errorf("白名单内应回落到 ETF 名称，实际 name=%q err=%v", nm, err)
	}
	if _, err := s.SellName(ctx, "600908.SH"); err == nil {
		t.Errorf("个股缺行必须报错（数据链问题不许静默兜名字）")
	}

	// stock_basic 有个股行时优先用库里的名字，回落不参与。
	if err := st.MarketRepo().UpsertStockBasic(ctx, model.StockBasic{
		TsCode: "600908.SH", Name: "无锡银行", ListStatus: "L",
	}); err != nil {
		t.Fatalf("写 stock_basic 失败: %v", err)
	}
	if nm, err := s.SellName(ctx, "600908.SH"); err != nil || nm != "无锡银行" {
		t.Errorf("库内名称优先，实际 name=%q err=%v", nm, err)
	}
}

// TestSellNameFallbackNoName 白名单只写了代码（无名称）时不得凭空造名字：
// 报错比发一张"未知标的"的指令单好。
func TestSellNameFallbackNoName(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/name2.db")
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer st.Close()
	s := NewService(st, &ticket.Ledger{}, store.MarketGate{Enabled: false},
		ETFUniverse{"512480.SH": ""})
	if _, err := s.SellName(context.Background(), "512480.SH"); err == nil {
		t.Error("无名条目不该被当成有效回落")
	} else if errors.Is(err, context.Canceled) {
		t.Fatal("不应吞掉原始错误") // 断言 err 来自读取层而不是自造的哨兵
	}
}

// TestETFCandidateKeepsPriceRules 卖出规则对 ETF 的豁免只限排名淘汰一条；
// 用 EvalSell 逐条对比 ETF 与同参数个股，除 rank_out 外结论必须一致。
func TestETFCandidateKeepsPriceRules(t *testing.T) {
	p := risk.DefaultParams(model.Fen(1182679))
	cases := []struct {
		name      string
		lastClose model.Fen
		high      model.Fen
		wantRule  string
	}{
		{"止损", 90, 100, RuleStopLoss},
		{"止盈", 116, 116, RuleTakeProfit},
		{"移动止盈", 142, 160, RuleTrailingStop},
	}
	for _, c := range cases {
		etf := HoldingCtx{Pos: model.Position{TsCode: "588000.SH", TotalQty: model.Qty(100),
			CostPrice: 100, HighPrice: c.high}, LastClose: c.lastClose, IsETF: true}
		stock := etf
		stock.IsETF = false
		e, s := EvalSell("20261009", etf, p, 0, 0), EvalSell("20261009", stock, p, 0, 0)
		if e == nil || e.Rule != c.wantRule {
			t.Errorf("%s：ETF 侧应触发 %s，实际 %+v", c.name, c.wantRule, e)
		}
		if s == nil || s.Rule != c.wantRule {
			t.Errorf("%s：个股侧应触发 %s，实际 %+v", c.name, c.wantRule, s)
		}
	}
}

// TestEvalMarketBadUnchangedForETF 大盘恶化那条规则对 ETF 照旧（它保护的是敞口本身），
// 只有排名淘汰被豁免。门槛开启时指数跌破均线 → 两侧都出信号。
func TestEvalMarketBadUnchangedForETF(t *testing.T) {
	p := risk.DefaultParams(model.Fen(1182679))
	h := etfHolding(true)
	sig := EvalSell("20261009", h, p, model.Fen(400000), model.Fen(420000))
	if sig == nil || sig.Rule != RuleMarketBad {
		t.Errorf("ETF 持仓仍需响应大盘恶化，实际 %+v", sig)
	}
	if strings.Contains(sig.Reason, "排名") {
		t.Errorf("原因不该混入排名淘汰：%q", sig.Reason)
	}
}
