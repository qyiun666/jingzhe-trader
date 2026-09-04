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

// ValidTicketStatus 状态名是否在状态机内（对外接口入参白名单）。
func ValidTicketStatus(s string) bool {
	_, ok := ticketTransitions[TicketStatus(s)]
	return ok
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

// ===================== 邮件类型 MailType =====================

// MailType 五类邮件：M1 次日指令 / M2 盘前提醒 / M3 盘中紧急 / M5 日报 / M6 异常告警。
//
// 原 M4「档位变更立即发」已删（2026-09-04）：没有任何一处发送过它，
// 而当前档位就在每封邮件顶部三行的第一行、日报也按档位单列 —— 变更当天必然看得见。
type MailType string

const (
	MailM1 MailType = "M1"
	MailM2 MailType = "M2"
	MailM3 MailType = "M3"
	MailM5 MailType = "M5"
	MailM6 MailType = "M6"
)

// OnceDaily 该类型当天只该送达一封。
//
// M1/M2/M5 各绑定一个固定的当日时刻（17:00 待买卖 / 09:00 计划 / 18:00 日报），
// 同一天重跑这个任务不该再发第二封 —— `trigger_task` 手工补跑一次就多一封，
// 是收件箱被刷爆的成因。M3（盘中止损，每轮内容随本轮新单变化）与
// M6（告警，一天可有多条不同 code）不在此列：它们重复是有信息量的。
func (t MailType) OnceDaily() bool {
	switch t {
	case MailM1, MailM2, MailM5:
		return true
	}
	return false
}
