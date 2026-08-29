package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// AlertLevel 通知级别
const (
	AlertLevelInfo    = "info"    // 常规信息
	AlertLevelWarning = "warning" // 警告
	AlertLevelUrgent  = "urgent"  // 紧急
	AlertLevelSuccess = "success" // 成功
)

// AlertStatus 通知状态 (是否已被 Agent 读取)
const (
	AlertStatusUnread = "unread" // 未读
	AlertStatusRead   = "read"   // 已读
)

// AgentAlert 智能体通知记录
type AgentAlert struct {
	ID        int64  `json:"id" db:"id"`
	TradeDate string `json:"trade_date" db:"trade_date"`
	JobName   string `json:"job_name" db:"job_name"`     // 触发来源 (signal/report/intraday_monitor等)
	Level     string `json:"level" db:"level"`           // info/warning/urgent/success
	Title     string `json:"title" db:"title"`           // 通知标题
	Content   string `json:"content" db:"content"`       // 通知正文
	Status    string `json:"status" db:"status"`         // unread/read
	CreatedAt string `json:"created_at" db:"created_at"` // 创建时间
	ReadAt    string `json:"read_at" db:"read_at"`       // 读取时间 (Agent 读取后标记)
}

// AlertRepo 通知仓储
type AlertRepo struct {
	db *sqlx.DB
}

// NewAlertRepo 创建通知仓储
func NewAlertRepo(db *sqlx.DB) *AlertRepo {
	return &AlertRepo{db: db}
}

// Insert 插入通知记录
func (r *AlertRepo) Insert(alert *AgentAlert) (int64, error) {
	now := time.Now().Format(TimeLayout)
	if alert.Status == "" {
		alert.Status = AlertStatusUnread
	}
	if alert.CreatedAt == "" {
		alert.CreatedAt = now
	}
	res, err := r.db.Exec(`INSERT INTO agent_alert
		(trade_date, job_name, level, title, content, status, created_at, read_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		alert.TradeDate, alert.JobName, alert.Level, alert.Title,
		alert.Content, alert.Status, alert.CreatedAt, "")
	if err != nil {
		return 0, fmt.Errorf("插入通知记录失败: %w", err)
	}
	return res.LastInsertId()
}

// GetUnread 获取未读通知 (Agent 首次读取用)
func (r *AlertRepo) GetUnread() ([]AgentAlert, error) {
	var alerts []AgentAlert
	err := r.db.Select(&alerts,
		`SELECT * FROM agent_alert WHERE status = ? ORDER BY id DESC`,
		AlertStatusUnread)
	if err != nil {
		return nil, fmt.Errorf("查询未读通知失败: %w", err)
	}
	return alerts, nil
}

// GetByDate 获取指定日期的所有通知
func (r *AlertRepo) GetByDate(tradeDate string) ([]AgentAlert, error) {
	var alerts []AgentAlert
	err := r.db.Select(&alerts,
		`SELECT * FROM agent_alert WHERE trade_date = ? ORDER BY id DESC`, tradeDate)
	if err != nil {
		return nil, fmt.Errorf("查询通知失败: %w", err)
	}
	return alerts, nil
}

// GetRecent 获取最近 N 条通知
func (r *AlertRepo) GetRecent(limit int) ([]AgentAlert, error) {
	if limit <= 0 {
		limit = 20
	}
	var alerts []AgentAlert
	err := r.db.Select(&alerts,
		`SELECT * FROM agent_alert ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询最近通知失败: %w", err)
	}
	return alerts, nil
}

// MarkAsRead 将指定 ID 的通知标记为已读
func (r *AlertRepo) MarkAsRead(id int64) error {
	now := time.Now().Format(TimeLayout)
	res, err := r.db.Exec(`UPDATE agent_alert SET status = ?, read_at = ? WHERE id = ?`,
		AlertStatusRead, now, id)
	if err != nil {
		return fmt.Errorf("标记已读失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("通知不存在(id=%d)", id)
	}
	return nil
}

// MarkAllRead 将所有未读通知标记为已读
func (r *AlertRepo) MarkAllRead() (int64, error) {
	now := time.Now().Format(TimeLayout)
	res, err := r.db.Exec(`UPDATE agent_alert SET status = ?, read_at = ? WHERE status = ?`,
		AlertStatusRead, now, AlertStatusUnread)
	if err != nil {
		return 0, fmt.Errorf("批量标记已读失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
