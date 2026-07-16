package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// PortfolioSyncItem 持仓同步条目，用于持久化存储
type PortfolioSyncItem struct {
	TsCode       string  `json:"ts_code" db:"ts_code"`
	TotalQty     int     `json:"total_qty" db:"total_qty"`
	AvailableQty int     `json:"available_qty" db:"available_qty"`
	CostPrice    float64 `json:"cost_price" db:"cost_price"`
	AvgPrice     float64 `json:"avg_price" db:"avg_price"`
	UpdatedAt    string  `json:"updated_at" db:"updated_at"`
}

// PortfolioRepo 持仓持久化仓储
// 负责 portfolio 和 portfolio_meta 表的读写
type PortfolioRepo struct {
	db *sqlx.DB
}

// NewPortfolioRepo 构造 PortfolioRepo
func NewPortfolioRepo(db *sqlx.DB) *PortfolioRepo {
	return &PortfolioRepo{db: db}
}

const portfolioInsertSQL = `INSERT INTO portfolio
	(ts_code, total_qty, available_qty, cost_price, avg_price, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)`

const portfolioUpsertSQL = `INSERT INTO portfolio
	(ts_code, total_qty, available_qty, cost_price, avg_price, updated_at)
	VALUES (?, ?, ?, ?, ?, ?)
	ON CONFLICT(ts_code) DO UPDATE SET
		total_qty     = excluded.total_qty,
		available_qty = excluded.available_qty,
		cost_price    = excluded.cost_price,
		avg_price     = excluded.avg_price,
		updated_at    = excluded.updated_at`

// SyncPortfolio 清空旧持仓数据，批量插入新持仓（事务保证原子性）
func (r *PortfolioRepo) SyncPortfolio(positions []PortfolioSyncItem) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 清空旧数据
	if _, err := tx.Exec("DELETE FROM portfolio"); err != nil {
		return fmt.Errorf("清空持仓失败: %w", err)
	}

	// 批量插入新持仓
	if len(positions) > 0 {
		stmt, err := tx.Preparex(portfolioInsertSQL)
		if err != nil {
			return fmt.Errorf("预编译插入语句失败: %w", err)
		}
		defer stmt.Close()

		now := time.Now().Format("2006-01-02 15:04:05")
		for _, p := range positions {
			// 如果条目未设置更新时间，使用当前时间
			updatedAt := p.UpdatedAt
			if updatedAt == "" {
				updatedAt = now
			}
			if _, err := stmt.Exec(
				p.TsCode, p.TotalQty, p.AvailableQty,
				p.CostPrice, p.AvgPrice, updatedAt,
			); err != nil {
				return fmt.Errorf("插入持仓失败(ts_code=%s): %w", p.TsCode, err)
			}
		}
	}

	return tx.Commit()
}

// UpsertPosition 插入或更新单只股票的持仓
func (r *PortfolioRepo) UpsertPosition(pos PortfolioSyncItem) error {
	updatedAt := pos.UpdatedAt
	if updatedAt == "" {
		updatedAt = time.Now().Format("2006-01-02 15:04:05")
	}
	_, err := r.db.Exec(portfolioUpsertSQL,
		pos.TsCode, pos.TotalQty, pos.AvailableQty,
		pos.CostPrice, pos.AvgPrice, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("插入/更新持仓失败(ts_code=%s): %w", pos.TsCode, err)
	}
	return nil
}

// RemovePosition 删除单只股票的持仓
func (r *PortfolioRepo) RemovePosition(tsCode string) error {
	_, err := r.db.Exec("DELETE FROM portfolio WHERE ts_code = ?", tsCode)
	if err != nil {
		return fmt.Errorf("删除持仓失败(ts_code=%s): %w", tsCode, err)
	}
	return nil
}

// GetAllPositions 查询所有持仓
func (r *PortfolioRepo) GetAllPositions() ([]PortfolioSyncItem, error) {
	var positions []PortfolioSyncItem
	err := r.db.Select(&positions,
		`SELECT ts_code, total_qty, available_qty, cost_price, avg_price, updated_at
		 FROM portfolio ORDER BY ts_code ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询所有持仓失败: %w", err)
	}
	return positions, nil
}

// GetPosition 查询单只股票的持仓
func (r *PortfolioRepo) GetPosition(tsCode string) (*PortfolioSyncItem, error) {
	var pos PortfolioSyncItem
	err := r.db.Get(&pos,
		`SELECT ts_code, total_qty, available_qty, cost_price, avg_price, updated_at
		 FROM portfolio WHERE ts_code = ?`, tsCode)
	if err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询持仓失败(ts_code=%s): %w", tsCode, err)
	}
	return &pos, nil
}

// SetMeta 设置持仓元数据（如 initial_capital）
func (r *PortfolioRepo) SetMeta(key, value string) error {
	_, err := r.db.Exec(
		`INSERT INTO portfolio_meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	if err != nil {
		return fmt.Errorf("设置元数据失败(key=%s): %w", key, err)
	}
	return nil
}

// GetMeta 获取持仓元数据
func (r *PortfolioRepo) GetMeta(key string) (string, error) {
	var value string
	err := r.db.Get(&value, `SELECT value FROM portfolio_meta WHERE key = ?`, key)
	if err != nil {
		if isNoRowsErr(err) {
			return "", nil
		}
		return "", fmt.Errorf("获取元数据失败(key=%s): %w", key, err)
	}
	return value, nil
}
