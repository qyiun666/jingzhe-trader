package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// DebateReview 辩论决策复盘记录 (agent_debate 结论的事后验证)
// correct: 1=决策方向与实际涨跌一致, 0=不一致
type DebateReview struct {
	ID         int64   `json:"id" db:"id"`
	DebateID   int64   `json:"debate_id" db:"debate_id"`
	TradeDate  string  `json:"trade_date" db:"trade_date"`
	TsCode     string  `json:"ts_code" db:"ts_code"`
	Decision   string  `json:"decision" db:"decision"`
	Confidence float64 `json:"confidence" db:"confidence"`
	BaseClose  float64 `json:"base_close" db:"base_close"`
	ReviewDate string  `json:"review_date" db:"review_date"`
	LastClose  float64 `json:"last_close" db:"last_close"`
	RetPct     float64 `json:"ret_pct" db:"ret_pct"`
	Correct    int     `json:"correct" db:"correct"`
}

// DebateReviewRepo 辩论复盘仓储
type DebateReviewRepo struct {
	db *sqlx.DB
}

// NewDebateReviewRepo 构造 DebateReviewRepo
func NewDebateReviewRepo(db *sqlx.DB) *DebateReviewRepo {
	return &DebateReviewRepo{db: db}
}

// Insert 插入复盘记录 (debate_id 唯一索引兜底防重)
func (r *DebateReviewRepo) Insert(review *DebateReview) (int64, error) {
	res, err := r.db.Exec(`INSERT INTO agent_debate_review
		(debate_id, trade_date, ts_code, decision, confidence, base_close, review_date, last_close, ret_pct, correct)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		review.DebateID, review.TradeDate, review.TsCode, review.Decision, review.Confidence,
		review.BaseClose, review.ReviewDate, review.LastClose, review.RetPct, review.Correct)
	if err != nil {
		return 0, fmt.Errorf("插入辩论复盘失败: %w", err)
	}
	return res.LastInsertId()
}

// GetRecentByCode 查询指定股票最近 N 条复盘记录 (按决策日倒序, 供辩论上下文反思注入)
func (r *DebateReviewRepo) GetRecentByCode(tsCode string, limit int) ([]DebateReview, error) {
	if limit <= 0 {
		limit = 5
	}
	var reviews []DebateReview
	err := r.db.Select(&reviews,
		`SELECT * FROM agent_debate_review WHERE ts_code = ? ORDER BY trade_date DESC LIMIT ?`,
		tsCode, limit)
	if err != nil {
		return nil, fmt.Errorf("查询辩论复盘失败: %w", err)
	}
	return reviews, nil
}
