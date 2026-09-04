package notify

import (
	"fmt"
	"strings"
	"time"
)

// GoalBrief 邮件顶部三行的"目标还差多少"数据源（渲染层转元，金额不进模板层计算）。
type GoalBrief struct {
	Gear        string  // 当前档位 G1/G2/G3
	GearLabel   string  // 中文标签
	ProfitLock  bool    // 是否锁利
	ProgressPct float64 // 季度进度（百分比数值，如 3.2）
	TargetPct   float64 // 季度目标（如 15.0）
	PaceGapPct  float64 // 时间进度 - 目标进度（>0 落后）
	CashYuan    float64 // 账户现金（元）
	TotalYuan   float64 // 总资产（元）
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

// briefLines 顶部三行：做什么 / 多少钱 / 目标还差多少（验收 §10.5-6）。
// 返回 nil 表示没有概要可写（紧急/告警邮件），此时整段不渲染。
func (b GoalBrief) empty() bool {
	return b.Gear == "" && b.TotalYuan == 0 && b.CashYuan == 0 && b.TargetPct == 0
}

func briefLines(b GoalBrief) []string {
	if b.empty() {
		return nil
	}
	lock := ""
	if b.ProfitLock {
		lock = "（已锁利）"
	}
	return []string{
		fmt.Sprintf("【做什么】当前档位 %s(%s)%s，请查看下方指令/提示并在有效期内人工执行",
			b.Gear, b.GearLabel, lock),
		fmt.Sprintf("【多少钱】总资产 %.2f 元，现金 %.2f 元", b.TotalYuan, b.CashYuan),
		fmt.Sprintf("【目标还差多少】季度进度 %.1f%% / 目标 %.1f%%，还差 %.1f 个百分点（时间进度差 %+.1f%%）",
			b.ProgressPct, b.TargetPct, max(0, b.TargetPct-b.ProgressPct), b.PaceGapPct),
	}
}

// render 通用拼装：主题 + 顶部三行 + 明细区块。
func render(subject string, b GoalBrief, sections []string) (string, string) {
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
func RenderM1(lines []TicketLine, b GoalBrief, noTicketReason string) (string, string) {
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

// RenderM2 M2 盘前提醒（条件触发：有效票/接近止损/未回执）。
func RenderM2(items []string, b GoalBrief) (string, string) {
	subject := fmt.Sprintf("[惊蛰][盘前] %d 项待关注", len(items))
	sections := []string{"== 盘前提醒 ==\n" + strings.Join(items, "\n")}
	return render(subject, b, sections)
}

// RenderM3 M3 盘中紧急（止损/移动止盈触发，立即发）。
func RenderM3(lines []TicketLine, reason string) (string, string) {
	b := GoalBrief{} // 紧急邮件以动作为主，顶部行仍保留结构
	var sb strings.Builder
	sb.WriteString("== 盘中紧急 ==\n" + reason + "\n")
	for _, l := range lines {
		fmt.Fprintf(&sb, "%s %s（%s）%d 股，参考价 %.2f，有效期至 %s\n", l.TsCode, l.Name, l.DirLabel, l.Qty, l.Price, l.ValidUntil)
	}
	subject := fmt.Sprintf("[惊蛰][紧急] 盘中触发 %d 笔卖出", len(lines))
	return render(subject, b, []string{sb.String()})
}

// RenderM5 M5 每日报告（心跳必发；degraded 与 success 分列）。
func RenderM5(tradeDate string, selfcheckBlock string, okJobs, degradedJobs, failedJobs []string, b GoalBrief) (string, string) {
	subject := fmt.Sprintf("[惊蛰][日报] %s", tradeDate)
	var sb strings.Builder
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
	b := GoalBrief{}
	return render(subject, b, []string{fmt.Sprintf("== 告警 %s（%s）==\n%s", code, level, content)})
}
