package notify

import (
	"strings"
	"testing"
)

// TestRenderM2ShowsConclusion 盘前邮件必须带上"昨日收盘结论"段：
// 用户要靠它知道今天为什么没指令 / 有哪几笔要执行。
func TestRenderM2ShowsConclusion(t *testing.T) {
	subject, body := RenderM2([]string{"持仓 600000.SH 100 股"}, nil,
		Brief{CashYuan: 1000, TotalYuan: 20000},
		"大盘在 MA60 下方，当日按规则关闭买入漏斗（非故障）")
	if !strings.Contains(subject, "盘前") {
		t.Errorf("主题异常: %q", subject)
	}
	if !strings.Contains(body, "昨日收盘结论") || !strings.Contains(body, "MA60") {
		t.Errorf("正文缺结论段:\n%s", body)
	}
	// 结论为空时不渲染该段（紧急/告警邮件复用同一 render）
	_, body2 := RenderM2([]string{"x"}, nil, Brief{}, "")
	if strings.Contains(body2, "昨日收盘结论") {
		t.Errorf("空结论不应渲染结论段:\n%s", body2)
	}
}

// TestCondOrderBlockBuy 买单换算：触发=参考价+0.01，委托=参考价+0.02，
// 金额=数量×委托限价；量纲按分入库、按元展示（593 分 = 5.93 元）。
func TestCondOrderBlockBuy(t *testing.T) {
	block := CondOrderBlock(TicketLine{
		TsCode: "600908.SH", Name: "无锡银行", Direction: "buy", DirLabel: "买入",
		Qty: 100, Price: 5.93,
	})
	for _, want := range []string{
		"买入页-定价买",
		"监控价达到/穿过 5.94 元",
		"自定义 限价 5.95 元",
		"委托数量：100 股",
		"监控有效期至：1日",
		"全关",
		"预计金额：595.00 元",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("买单参数块缺 %q:\n%s", want, block)
		}
	}
	if strings.Count(block, "\n") != 9 {
		t.Errorf("参数块应为 10 行（标题+9 项）:\n%s", block)
	}
}

// TestCondOrderBlockSell 卖单（止损/止盈/常规同一口径）：触发=目标价，委托=目标价-0.02。
func TestCondOrderBlockSell(t *testing.T) {
	block := CondOrderBlock(TicketLine{
		TsCode: "600908.SH", Name: "无锡银行", Direction: "sell", DirLabel: "卖出",
		Qty: 300, Price: 6.80,
	})
	for _, want := range []string{
		"卖出页-定价卖",
		"监控价达到/穿过 6.80 元",
		"自定义 限价 6.78 元",
		"预计金额：2034.00 元",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("卖单参数块缺 %q:\n%s", want, block)
		}
	}
}

// TestRenderM2EmbedsCondBlocks M2 有有效指令单时必须内嵌参数块段；无单不渲染该段。
func TestRenderM2EmbedsCondBlocks(t *testing.T) {
	tk := TicketLine{TsCode: "600908.SH", Name: "无锡银行", Direction: "buy", DirLabel: "买入", Qty: 100, Price: 5.93, ValidUntil: "2026-10-09T15:00"}
	_, body := RenderM2([]string{"待执行 #7"}, []TicketLine{tk}, Brief{CashYuan: 1, TotalYuan: 2}, "")
	if !strings.Contains(body, "条件单速填参数") || !strings.Contains(body, "限价 5.95 元") {
		t.Errorf("M2 缺条件单参数块:\n%s", body)
	}
	_, body2 := RenderM2([]string{"持仓"}, nil, Brief{}, "")
	if strings.Contains(body2, "条件单速填") {
		t.Errorf("无指令单不应渲染参数块段:\n%s", body2)
	}
}

// TestRenderM3EmbedsCondBlocks 盘中紧急卖单逐单附参数块。
func TestRenderM3EmbedsCondBlocks(t *testing.T) {
	_, body := RenderM3([]TicketLine{{
		TsCode: "600908.SH", Name: "无锡银行", Direction: "sell", DirLabel: "卖出",
		Qty: 100, Price: 5.46, ValidUntil: "2026-10-09T15:00",
	}}, "盘中触发卖出")
	if !strings.Contains(body, "卖出页-定价卖") || !strings.Contains(body, "限价 5.44 元") {
		t.Errorf("M3 缺条件单参数块:\n%s", body)
	}
}

// TestRenderM5ShowsConclusion 日报必须把"今日结论"放在最前。
func TestRenderM5ShowsConclusion(t *testing.T) {
	_, body := RenderM5("20260903", "自检ok", []string{"morning_plan"}, nil, nil,
		Brief{CashYuan: 1, TotalYuan: 2}, "今日 2 笔指令")
	if !strings.Contains(body, "今日结论") || !strings.Contains(body, "今日 2 笔指令") {
		t.Errorf("日报缺今日结论:\n%s", body)
	}
	if strings.Index(body, "今日结论") > strings.Index(body, "任务执行") {
		t.Errorf("今日结论应在任务执行之前")
	}
}
