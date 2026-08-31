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
	TodayBought  int     `json:"today_bought" db:"today_bought"` // 今日买入量 (T+1: 次日结转时可卖)
	HighPrice    float64 `json:"high_price" db:"high_price"`     // 持仓期间历史最高价 (移动止盈用)
	CostPrice    float64 `json:"cost_price" db:"cost_price"`
	UpdatedAt    string  `json:"updated_at" db:"updated_at"`
}

// PortfolioRepo 持仓持久化仓储
// 负责 portfolio 持仓表与 config_kv 配置表的读写
type PortfolioRepo struct {
	db *sqlx.DB
}

// NewPortfolioRepo 构造 PortfolioRepo
func NewPortfolioRepo(db *sqlx.DB) *PortfolioRepo {
	return &PortfolioRepo{db: db}
}

const portfolioInsertSQL = `INSERT INTO portfolio
	(ts_code, total_qty, available_qty, today_bought, high_price, cost_price, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)`

const portfolioUpsertSQL = `INSERT INTO portfolio
	(ts_code, total_qty, available_qty, today_bought, high_price, cost_price, updated_at)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(ts_code) DO UPDATE SET
		total_qty     = excluded.total_qty,
		available_qty = excluded.available_qty,
		today_bought  = excluded.today_bought,
		high_price    = excluded.high_price,
		cost_price    = excluded.cost_price,
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

		now := time.Now().Format(TimeLayout)
		for _, p := range positions {
			// 如果条目未设置更新时间，使用当前时间
			updatedAt := p.UpdatedAt
			if updatedAt == "" {
				updatedAt = now
			}
			if _, err := stmt.Exec(
				p.TsCode, p.TotalQty, p.AvailableQty, p.TodayBought, p.HighPrice,
				p.CostPrice, updatedAt,
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
		updatedAt = time.Now().Format(TimeLayout)
	}
	_, err := r.db.Exec(portfolioUpsertSQL,
		pos.TsCode, pos.TotalQty, pos.AvailableQty, pos.TodayBought, pos.HighPrice,
		pos.CostPrice, updatedAt,
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
		`SELECT ts_code, total_qty, available_qty, today_bought, high_price, cost_price, updated_at
		 FROM portfolio ORDER BY ts_code ASC`)
	if err != nil {
		return nil, fmt.Errorf("查询所有持仓失败: %w", err)
	}
	return positions, nil
}

// GetPosition 查询单只股票的持仓
func (r *PortfolioRepo) GetPosition(tsCode string) (*PortfolioSyncItem, error) {
	var pos PortfolioSyncItem
	found, err := getOne(r.db,
		`SELECT ts_code, total_qty, available_qty, today_bought, high_price, cost_price, updated_at
		 FROM portfolio WHERE ts_code = ?`, &pos, "查询持仓失败", tsCode)
	if err != nil || !found {
		return nil, err
	}
	return &pos, nil
}

// SettleT1 每日开盘前结转: 昨日买入转为可卖, today_bought 清零
func (r *PortfolioRepo) SettleT1() error {
	_, err := r.db.Exec("UPDATE portfolio SET available_qty = total_qty, today_bought = 0")
	if err != nil {
		return fmt.Errorf("T+1持仓结转失败: %w", err)
	}
	return nil
}

// UpdateHighPrice 更新持仓期间历史最高价 (仅在新高时上调, 移动止盈用)
func (r *PortfolioRepo) UpdateHighPrice(tsCode string, price float64) error {
	_, err := r.db.Exec("UPDATE portfolio SET high_price = MAX(high_price, ?) WHERE ts_code = ?", price, tsCode)
	if err != nil {
		return fmt.Errorf("更新持仓最高价失败(ts_code=%s): %w", tsCode, err)
	}
	return nil
}

// SetMeta 设置配置键值 (如 initial_capital) — 写入 config_kv 表
func (r *PortfolioRepo) SetMeta(key, value string) error {
	_, err := r.db.Exec(
		`INSERT INTO config_kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().Format(TimeLayout),
	)
	if err != nil {
		return fmt.Errorf("设置配置失败(key=%s): %w", key, err)
	}
	return nil
}

// GetMeta 获取配置键值 (如 initial_capital)
func (r *PortfolioRepo) GetMeta(key string) (string, error) {
	var value string
	err := r.db.Get(&value, `SELECT value FROM config_kv WHERE key = ?`, key)
	if err != nil {
		if isNoRowsErr(err) {
			return "", nil
		}
		return "", fmt.Errorf("获取配置失败(key=%s): %w", key, err)
	}
	return value, nil
}
