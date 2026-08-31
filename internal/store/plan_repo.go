package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// 交易计划状态
const (
	PlanStatusPending   = "pending"   // 待确认
	PlanStatusConfirmed = "confirmed" // 已确认(等待执行/已手动执行)
	PlanStatusExecuted  = "executed"  // 已通过broker执行
	PlanStatusExpired   = "expired"   // 已过期
)

// 交易计划紧急度
const (
	PlanUrgencyNormal = "normal" // 常规调仓 (EOD信号)
	PlanUrgencyUrgent = "urgent" // 紧急 (盘中止损触发)
)

// TradePlan 交易计划
type TradePlan struct {
	ID        int64   `json:"id" db:"id"`
	TradeDate string  `json:"trade_date" db:"trade_date"`
	TsCode    string  `json:"ts_code" db:"ts_code"`
	Name      string  `json:"name" db:"name"`
	Direction string  `json:"direction" db:"direction"` // buy / sell
	Qty       int     `json:"qty" db:"qty"`
	RefPrice  float64 `json:"ref_price" db:"ref_price"`
	Reason    string  `json:"reason" db:"reason"`
	Strategy  string  `json:"strategy" db:"strategy"`
	Urgency   string  `json:"urgency" db:"urgency"`
	Status    string  `json:"status" db:"status"`
	CreatedAt string  `json:"created_at" db:"created_at"`
	UpdatedAt string  `json:"updated_at" db:"updated_at"`
}

// PlanRepo 交易计划仓储
type PlanRepo struct {
	db *sqlx.DB
}

// NewPlanRepo 创建交易计划仓储
func NewPlanRepo(db *sqlx.DB) *PlanRepo {
	return &PlanRepo{db: db}
}

// InsertPlan 插入交易计划
func (r *PlanRepo) InsertPlan(p *TradePlan) (int64, error) {
	now := time.Now().Format(TimeLayout)
	p.CreatedAt = now
	p.UpdatedAt = now
	if p.Status == "" {
		p.Status = PlanStatusPending
	}
	if p.Urgency == "" {
		p.Urgency = PlanUrgencyNormal
	}
	res, err := r.db.Exec(`INSERT INTO trade_plan
		(trade_date, ts_code, name, direction, qty, ref_price, reason, strategy, urgency, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.TradeDate, p.TsCode, p.Name, p.Direction, p.Qty, p.RefPrice, p.Reason, p.Strategy, p.Urgency, p.Status, p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return 0, fmt.Errorf("插入交易计划失败: %w", err)
	}
	return res.LastInsertId()
}

// ReplaceDayPlans 替换指定日期的常规计划 (EOD重跑时覆盖旧计划, urgent计划保留)
func (r *PlanRepo) ReplaceDayPlans(tradeDate string, plans []*TradePlan) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 只清理当天未处理的常规计划, 已确认/已执行/紧急计划不动
	if _, err := tx.Exec(`DELETE FROM trade_plan WHERE trade_date = ? AND urgency = ? AND status = ?`,
		tradeDate, PlanUrgencyNormal, PlanStatusPending); err != nil {
		return fmt.Errorf("清理旧计划失败: %w", err)
	}

	now := time.Now().Format(TimeLayout)
	for _, p := range plans {
		if p.Status == "" {
			p.Status = PlanStatusPending
		}
		if p.Urgency == "" {
			p.Urgency = PlanUrgencyNormal
		}
		if _, err := tx.Exec(`INSERT INTO trade_plan
			(trade_date, ts_code, name, direction, qty, ref_price, reason, strategy, urgency, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.TradeDate, p.TsCode, p.Name, p.Direction, p.Qty, p.RefPrice, p.Reason, p.Strategy, p.Urgency, p.Status, now, now); err != nil {
			return fmt.Errorf("插入交易计划失败: %w", err)
		}
	}
	return tx.Commit()
}

// GetPlansByDate 查询指定日期的交易计划
func (r *PlanRepo) GetPlansByDate(tradeDate string) ([]TradePlan, error) {
	var plans []TradePlan
	err := r.db.Select(&plans,
		`SELECT * FROM trade_plan WHERE trade_date = ? ORDER BY urgency DESC, id ASC`, tradeDate)
	if err != nil {
		return nil, fmt.Errorf("查询交易计划失败: %w", err)
	}
	return plans, nil
}

// GetOpenPlans 查询所有待处理计划 (pending/confirmed, 跨日期)
func (r *PlanRepo) GetOpenPlans() ([]TradePlan, error) {
	var plans []TradePlan
	err := r.db.Select(&plans,
		`SELECT * FROM trade_plan WHERE status IN (?, ?) ORDER BY urgency DESC, trade_date DESC, id ASC`,
		PlanStatusPending, PlanStatusConfirmed)
	if err != nil {
		return nil, fmt.Errorf("查询待处理交易计划失败: %w", err)
	}
	return plans, nil
}

// GetPlanByID 按ID查询交易计划
func (r *PlanRepo) GetPlanByID(id int64) (*TradePlan, error) {
	var p TradePlan
	if err := r.db.Get(&p, `SELECT * FROM trade_plan WHERE id = ?`, id); err != nil {
		return nil, fmt.Errorf("查询交易计划失败(id=%d): %w", id, err)
	}
	return &p, nil
}

// UpdatePlanStatus 更新交易计划状态
func (r *PlanRepo) UpdatePlanStatus(id int64, status string) error {
	now := time.Now().Format(TimeLayout)
	res, err := r.db.Exec(`UPDATE trade_plan SET status = ?, updated_at = ? WHERE id = ?`, status, now, id)
	if err != nil {
		return fmt.Errorf("更新交易计划状态失败(id=%d): %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("交易计划不存在(id=%d)", id)
	}
	return nil
}

// DeletePlan 删除指定计划 (人工成交确认后剔除; 成交审计在 action_log, 无需保留计划行)
func (r *PlanRepo) DeletePlan(id int64) error {
	res, err := r.db.Exec(`DELETE FROM trade_plan WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除交易计划失败(id=%d): %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("交易计划不存在(id=%d)", id)
	}
	return nil
}

// ExpireOldPlans 将指定日期之前仍pending的计划标记为过期
func (r *PlanRepo) ExpireOldPlans(beforeDate string) (int64, error) {
	now := time.Now().Format(TimeLayout)
	res, err := r.db.Exec(`UPDATE trade_plan SET status = ?, updated_at = ? WHERE trade_date < ? AND status = ?`,
		PlanStatusExpired, now, beforeDate, PlanStatusPending)
	if err != nil {
		return 0, fmt.Errorf("过期交易计划失败: %w", err)
	}
	return res.RowsAffected()
}
