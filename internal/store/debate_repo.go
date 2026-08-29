package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// DebateResult 智能体辩论结果
type DebateResult struct {
	ID          int64   `json:"id" db:"id"`
	TradeDate   string  `json:"trade_date" db:"trade_date"`
	TsCode      string  `json:"ts_code" db:"ts_code"`
	Name        string  `json:"name" db:"name"`
	Decision    string  `json:"decision" db:"decision"`
	Confidence  float64 `json:"confidence" db:"confidence"`
	PositionPct float64 `json:"position_pct" db:"position_pct"`
	StopPrice   float64 `json:"stop_price" db:"stop_price"`
	RiskLevel   string  `json:"risk_level" db:"risk_level"`
	BullArgs    string  `json:"bull_args" db:"bull_args"`
	BearArgs    string  `json:"bear_args" db:"bear_args"`
	RiskNote    string  `json:"risk_note" db:"risk_note"`
	Summary     string  `json:"summary" db:"summary"`
	CreatedAt   string  `json:"created_at" db:"created_at"`
}

type DebateRepo struct {
	db *sqlx.DB
}

func NewDebateRepo(db *sqlx.DB) *DebateRepo {
	return &DebateRepo{db: db}
}

func (r *DebateRepo) Insert(result *DebateResult) (int64, error) {
	now := time.Now().Format(TimeLayout)
	res, err := r.db.Exec(`INSERT INTO agent_debate
		(trade_date, ts_code, name, decision, confidence, position_pct, stop_price,
		 risk_level, bull_args, bear_args, risk_note, summary, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		result.TradeDate, result.TsCode, result.Name, result.Decision,
		result.Confidence, result.PositionPct, result.StopPrice,
		result.RiskLevel, result.BullArgs, result.BearArgs,
		result.RiskNote, result.Summary, now)
	if err != nil {
		return 0, fmt.Errorf("插入辩论结果失败: %w", err)
	}
	return res.LastInsertId()
}

func (r *DebateRepo) GetByDate(tradeDate string) ([]DebateResult, error) {
	var results []DebateResult
	err := r.db.Select(&results,
		`SELECT * FROM agent_debate WHERE trade_date = ? ORDER BY id DESC`, tradeDate)
	if err != nil {
		return nil, fmt.Errorf("查询辩论结果失败: %w", err)
	}
	return results, nil
}

func (r *DebateRepo) GetByCode(tsCode string, limit int) ([]DebateResult, error) {
	if limit <= 0 {
		limit = 5
	}
	var results []DebateResult
	err := r.db.Select(&results,
		`SELECT * FROM agent_debate WHERE ts_code = ? ORDER BY created_at DESC LIMIT ?`, tsCode, limit)
	if err != nil {
		return nil, fmt.Errorf("查询辩论结果失败: %w", err)
	}
	return results, nil
}

func (r *DebateRepo) GetRecent(limit int) ([]DebateResult, error) {
	if limit <= 0 {
		limit = 20
	}
	var results []DebateResult
	err := r.db.Select(&results,
		`SELECT * FROM agent_debate ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("查询最近辩论结果失败: %w", err)
	}
	return results, nil
}

// GetPreviousDebates 获取指定日期之前每个股票最近一次辩论结果 (用于决策变更对比)
// 返回 map[ts_code]DebateResult
func (r *DebateRepo) GetPreviousDebates(beforeDate string) (map[string]DebateResult, error) {
	query := `SELECT d.* FROM agent_debate d
		INNER JOIN (
			SELECT ts_code, MAX(id) AS max_id
			FROM agent_debate
			WHERE trade_date < ?
			GROUP BY ts_code
		) latest ON d.id = latest.max_id`
	var results []DebateResult
	if err := r.db.Select(&results, query, beforeDate); err != nil {
		return nil, fmt.Errorf("查询历史辩论结果失败: %w", err)
	}
	m := make(map[string]DebateResult, len(results))
	for _, r := range results {
		m[r.TsCode] = r
	}
	return m, nil
}

// HasDebatesOnDate 判断指定日期是否已有辩论记录 (用于检测是否已执行)
func (r *DebateRepo) HasDebatesOnDate(tradeDate string) (bool, error) {
	return existsRow(r.db, `SELECT COUNT(1) FROM agent_debate WHERE trade_date = ?`, tradeDate)
}

// GetPendingReview 获取待复盘的辩论结论:
// 仅限有方向的决策 (buy/sell/reject, hold 无方向不可验证), 未复盘过, 决策日 <= beforeDate
func (r *DebateRepo) GetPendingReview(beforeDate string, limit int) ([]DebateResult, error) {
	if limit <= 0 {
		limit = 100
	}
	var results []DebateResult
	err := r.db.Select(&results, `SELECT d.* FROM agent_debate d
		WHERE d.trade_date <= ? AND d.decision IN ('buy','sell','reject')
		AND NOT EXISTS (SELECT 1 FROM agent_debate_review v WHERE v.debate_id = d.id)
		ORDER BY d.trade_date ASC LIMIT ?`, beforeDate, limit)
	if err != nil {
		return nil, fmt.Errorf("查询待复盘辩论失败: %w", err)
	}
	return results, nil
}
