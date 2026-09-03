package model

import "fmt"

// ===================== 风险档位 Gear =====================

// Gear 风险档位：G1 标准 / G2 收紧 / G3 防守。
type Gear string

const (
	GearG1 Gear = "G1"
	GearG2 Gear = "G2"
	GearG3 Gear = "G3"
)

// Valid 是否为合法档位。
func (g Gear) Valid() bool {
	switch g {
	case GearG1, GearG2, GearG3:
		return true
	default:
		return false
	}
}

// Label 中文标签。
func (g Gear) Label() string {
	switch g {
	case GearG1:
		return "标准"
	case GearG2:
		return "收紧"
	case GearG3:
		return "防守"
	default:
		return "未知"
	}
}

// ParseGear 解析档位字符串，非法返回 error。
func ParseGear(s string) (Gear, error) {
	g := Gear(s)
	if !g.Valid() {
		return "", fmt.Errorf("非法风险档位: %q（应为 G1/G2/G3）", s)
	}
	return g, nil
}

// ===================== 指令单状态 TicketStatus =====================

// TicketStatus 指令单状态机：drafted → issued → {filled|skipped|expired}。
type TicketStatus string

const (
	TicketDrafted TicketStatus = "drafted"
	TicketIssued  TicketStatus = "issued"
	TicketFilled  TicketStatus = "filled"
	TicketSkipped TicketStatus = "skipped"
	TicketExpired TicketStatus = "expired"
)

// ticketTransitions 合法状态转移表（PRD P0-8）。
var ticketTransitions = map[TicketStatus][]TicketStatus{
	TicketDrafted: {TicketIssued, TicketSkipped, TicketExpired},
	TicketIssued:  {TicketFilled, TicketSkipped, TicketExpired},
	TicketFilled:  {},
	TicketSkipped: {},
	TicketExpired: {},
}

// CanTransition 判断 from → to 是否合法。
func (s TicketStatus) CanTransition(to TicketStatus) bool {
	for _, next := range ticketTransitions[s] {
		if next == to {
			return true
		}
	}
	return false
}

// IsTerminal 是否为终态。
func (s TicketStatus) IsTerminal() bool {
	return len(ticketTransitions[s]) == 0
}

// ===================== 买卖方向 Direction =====================

// Direction 买卖方向。
type Direction string

const (
	DirBuy  Direction = "buy"
	DirSell Direction = "sell"
)

// Valid 是否合法方向。
func (d Direction) Valid() bool {
	return d == DirBuy || d == DirSell
}

// Label 中文标签。
func (d Direction) Label() string {
	switch d {
	case DirBuy:
		return "买入"
	case DirSell:
		return "卖出"
	default:
		return "未知"
	}
}

// ===================== 告警级别 AlertLevel =====================

// AlertLevel 告警四级：info / warning / urgent / success（PRD P0-16）。
type AlertLevel string

const (
	AlertInfo    AlertLevel = "info"
	AlertWarning AlertLevel = "warning"
	AlertUrgent  AlertLevel = "urgent"
	AlertSuccess AlertLevel = "success"
)

// Valid 是否合法告警级别。
func (a AlertLevel) Valid() bool {
	switch a {
	case AlertInfo, AlertWarning, AlertUrgent, AlertSuccess:
		return true
	default:
		return false
	}
}

// ===================== 任务状态 JobStatus =====================

// JobStatus 任务状态：running / success / degraded / failed。
// degraded 为新增态，表示任务成功但产出物缺失/降级（D1）。
type JobStatus string

const (
	JobRunning  JobStatus = "running"
	JobSuccess  JobStatus = "success"
	JobDegraded JobStatus = "degraded"
	JobFailed   JobStatus = "failed"
)

// ===================== 邮件类型 MailType =====================

// MailType 六类邮件：M1 次日指令 / M2 盘前提醒 / M3 盘中紧急 / M4 档位变更 / M5 日报 / M6 异常。
type MailType string

const (
	MailM1 MailType = "M1"
	MailM2 MailType = "M2"
	MailM3 MailType = "M3"
	MailM4 MailType = "M4"
	MailM5 MailType = "M5"
	MailM6 MailType = "M6"
)
