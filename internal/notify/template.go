package notify

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Brief 邮件顶部账户摘要（渲染层转元，金额不进模板层计算）。
type Brief struct {
	CashYuan  float64 // 账户现金（元）
	TotalYuan float64 // 总资产（元）
}

// TicketLine 指令行（M1/M3 渲染输入，金额已是元）。
type TicketLine struct {
	TsCode     string
	Name       string
	Direction  string // buy/sell
	DirLabel   string // 买入/卖出
	Qty        int64
	Price      float64 // 参考价（元）
	ValidUntil string
	Reason     string
}

// CondOrderBlock 把一张指令单渲染成招商证券条件单 9 项速填参数块（格式对齐组长群通知约定）。
// 换算规则（与参考价量纲无关，输入 Price 为元，内部按分取整避免浮点漂移）：
//   - 买单 = 买入页-定价买：触发 = 参考价+0.01，委托 = 自定义限价 参考价+0.02
//   - 卖单（止损/移动止盈/止盈/常规）= 卖出页-定价卖：触发 = 参考价（目标价），委托 = 参考价-0.02
//   - 数量 = 指令单数量；监控有效期 = 1 日；高级设置全关；预计金额 = 数量×委托限价
func CondOrderBlock(t TicketLine) string {
	fen := int64(math.Round(t.Price * 100))
	trigFen, orderFen := fen+1, fen+2
	op, condType := "买入", "买入页-定价买"
	if t.Direction != "buy" {
		trigFen, orderFen = fen, fen-2
		op, condType = "卖出", "卖出页-定价卖"
	}
	return fmt.Sprintf(
		"【条件单速填 %s %s】\n"+
			"1. 操作：%s\n"+
			"2. 条件单类型：%s\n"+
			"3. 触发条件：监控价达到/穿过 %.2f 元\n"+
			"4. 委托设置：自定义 限价 %.2f 元\n"+
			"5. 委托数量：%d 股\n"+
			"6. 监控有效期至：1日\n"+
			"7. 高级设置：监控时段/偏差控制/延迟确认 全关\n"+
			"8. 预计金额：%.2f 元\n"+
			"9. 成交后：群里报成交价+数量",
		t.TsCode, t.Name, op, condType,
		float64(trigFen)/100, float64(orderFen)/100, t.Qty,
		float64(orderFen)*float64(t.Qty)/100)
}

// briefLines 顶部账户摘要：多少钱。
// 返回 nil 表示没有概要可写（紧急/告警邮件），此时整段不渲染。
func (b Brief) empty() bool {
	return b.TotalYuan == 0 && b.CashYuan == 0
}

func briefLines(b Brief) []string {
	if b.empty() {
		return nil
	}
	return []string{
		fmt.Sprintf("【多少钱】总资产 %.2f 元，现金 %.2f 元", b.TotalYuan, b.CashYuan),
	}
}

// render 通用拼装：主题 + 顶部三行 + 明细区块。
func render(subject string, b Brief, sections []string) (string, string) {
	var body strings.Builder
	for _, line := range briefLines(b) {
		body.WriteString(line)
		body.WriteString("\n")
	}
	body.WriteString("\n")
	for _, s := range sections {
		body.WriteString(s)
		body.WriteString("\n\n")
	}
	fmt.Fprintf(&body, "—— 惊蛰 · %s", time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04"))
	return subject, body.String()
}

// RenderM1 M1 次日指令邮件（有票：明细；无票：原因说明）。
func RenderM1(lines []TicketLine, b Brief, noTicketReason string) (string, string) {
	subject := fmt.Sprintf("[惊蛰][指令] 次日操作 %d 笔", len(lines))
	if len(lines) == 0 {
		subject = "[惊蛰][指令] 今日无指令"
	}
	var sections []string
	if len(lines) == 0 {
		sections = append(sections, "今日无指令。原因："+noTicketReason)
	} else {
		var sb strings.Builder
		sb.WriteString("== 明日指令（请人工在券商 App 执行，有效期见下）==\n")
		for i, l := range lines {
			fmt.Fprintf(&sb, "%d. %s %s（%s）%d 股，参考价 %.2f 元，有效期至 %s\n   回执话术：「%s %s %d 股，指令单已回执」\n   理由：%s",
				i+1, l.TsCode, l.Name, l.DirLabel, l.Qty, l.Price, l.ValidUntil,
				l.TsCode, l.DirLabel, l.Qty, l.Reason)
			sb.WriteString("\n")
		}
		sections = append(sections, sb.String())
	}
	return render(subject, b, sections)
}

// RenderM2 M2 盘前提醒（当日计划：待执行指令 + 持仓 + 昨日结论 + 条件单速填参数）。
// conclusion 是"最近一次收盘流水线给当天留下了什么"的人话结论（可为空串，则整段不渲染）。
// tickets 是当日待执行指令单（可为空，则参数块段不渲染）。
func RenderM2(items []string, tickets []TicketLine, b Brief, conclusion string) (string, string) {
	subject := fmt.Sprintf("[惊蛰][盘前] %d 项待关注", len(items))
	var sections []string
	if conclusion != "" {
		sections = append(sections, "== 昨日收盘结论 ==\n"+conclusion)
	}
	sections = append(sections, "== 盘前提醒 ==\n"+strings.Join(items, "\n"))
	if len(tickets) > 0 {
		var sb strings.Builder
		sb.WriteString("== 条件单速填参数（有效指令单逐张）==\n")
		for i, t := range tickets {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(CondOrderBlock(t))
			sb.WriteString("\n")
		}
		sections = append(sections, sb.String())
	}
	return render(subject, b, sections)
}

// RenderM3 M3 盘中紧急（止损/移动止盈触发，立即发；逐单附条件单速填参数）。
func RenderM3(lines []TicketLine, reason string) (string, string) {
	b := Brief{} // 紧急邮件以动作为主，顶部行仍保留结构
	var sb strings.Builder
	sb.WriteString("== 盘中紧急 ==\n" + reason + "\n")
	for _, l := range lines {
		fmt.Fprintf(&sb, "%s %s（%s）%d 股，参考价 %.2f，有效期至 %s\n", l.TsCode, l.Name, l.DirLabel, l.Qty, l.Price, l.ValidUntil)
		sb.WriteString(CondOrderBlock(l))
		sb.WriteString("\n")
	}
	subject := fmt.Sprintf("[惊蛰][紧急] 盘中触发 %d 笔卖出", len(lines))
	return render(subject, b, []string{sb.String()})
}

// RenderM5 M5 每日报告（心跳必发；degraded 与 success 分列）。
// conclusion 是"今天为什么买/不买"的人话结论（当日流水线给出，可为空串）。
func RenderM5(tradeDate string, selfcheckBlock string, okJobs, degradedJobs, failedJobs []string, b Brief, conclusion string) (string, string) {
	subject := fmt.Sprintf("[惊蛰][日报] %s", tradeDate)
	var sb strings.Builder
	if conclusion != "" {
		sb.WriteString("== 今日结论 ==\n" + conclusion + "\n\n")
	}
	sb.WriteString("== 任务执行 ==\n")
	fmt.Fprintf(&sb, "成功 %d：%s\n", len(okJobs), strings.Join(okJobs, ", "))
	if len(degradedJobs) > 0 {
		// degraded 单列，绝不与绿灯混排（验收 §10.5-12）
		fmt.Fprintf(&sb, "降级 %d：%s\n", len(degradedJobs), strings.Join(degradedJobs, ", "))
	}
	if len(failedJobs) > 0 {
		fmt.Fprintf(&sb, "失败 %d：%s\n", len(failedJobs), strings.Join(failedJobs, ", "))
	}
	sections := []string{sb.String(), selfcheckBlock}
	return render(subject, b, sections)
}

// RenderM6 M6 异常告警（RaiseUrgent 立即发）。
func RenderM6(level, code, title, content string) (string, string) {
	subject := fmt.Sprintf("[惊蛰][%s] %s：%s", level, code, title)
	b := Brief{}
	return render(subject, b, []string{fmt.Sprintf("== 告警 %s（%s）==\n%s", code, level, content)})
}
