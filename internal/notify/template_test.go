package notify

import (
	"strings"
	"testing"
)

// TestRenderM2ShowsConclusion 盘前邮件必须带上"昨日收盘结论"段：
// 用户要靠它知道今天为什么没指令 / 有哪几笔要执行。
func TestRenderM2ShowsConclusion(t *testing.T) {
	subject, body := RenderM2([]string{"持仓 600000.SH 100 股"},
		Brief{CashYuan: 1000, TotalYuan: 20000},
		"大盘在 MA60 下方，当日按规则关闭买入漏斗（非故障）")
	if !strings.Contains(subject, "盘前") {
		t.Errorf("主题异常: %q", subject)
	}
	if !strings.Contains(body, "昨日收盘结论") || !strings.Contains(body, "MA60") {
		t.Errorf("正文缺结论段:\n%s", body)
	}
	// 结论为空时不渲染该段（紧急/告警邮件复用同一 render）
	_, body2 := RenderM2([]string{"x"}, Brief{}, "")
	if strings.Contains(body2, "昨日收盘结论") {
		t.Errorf("空结论不应渲染结论段:\n%s", body2)
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
