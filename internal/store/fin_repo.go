package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"

	"jingzhe-trader/internal/model"
)

// FinaRepo 财务指标仓储
type FinaRepo struct {
	db *sqlx.DB
}

// NewFinaRepo 构造 FinaRepo
func NewFinaRepo(db *sqlx.DB) *FinaRepo {
	return &FinaRepo{db: db}
}

const finaInsertSQL = `INSERT OR REPLACE INTO fina_indicator
	(ts_code, end_date, ann_date, eps, roe, grossprofit_margin, netprofit_margin,
	 debt_to_assets, netprofit_yoy, or_yoy, bps)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

const finaSelectCols = `ts_code, end_date, ann_date, eps, roe, grossprofit_margin, netprofit_margin,
	debt_to_assets, netprofit_yoy, or_yoy, bps`

// BatchInsert 批量插入财务指标(已存在则覆盖, 主键为 ts_code + end_date)
func (r *FinaRepo) BatchInsert(indicators []model.FinaIndicator) error {
	if len(indicators) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Preparex(finaInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, f := range indicators {
		if _, err := stmt.Exec(
			f.TsCode, f.EndDate, f.AnnDate, f.EPS, f.ROE,
			f.GrossProfitMargin, f.NetProfitMargin, f.DebtToAssets,
			f.NetProfitYoy, f.ORYoy, f.BPS,
		); err != nil {
			return fmt.Errorf("插入财务指标失败(ts_code=%s end_date=%s): %w", f.TsCode, f.EndDate, err)
		}
	}
	return tx.Commit()
}

// GetByCode 查询指定股票的全部财务指标(按报告期升序)
func (r *FinaRepo) GetByCode(tsCode string) ([]model.FinaIndicator, error) {
	query := fmt.Sprintf(`SELECT %s FROM fina_indicator
		WHERE ts_code = ? ORDER BY end_date ASC`, finaSelectCols)
	var fis []model.FinaIndicator
	if err := r.db.Select(&fis, query, tsCode); err != nil {
		return nil, fmt.Errorf("查询财务指标失败: %w", err)
	}
	return fis, nil
}

// GetByCodeAndPeriod 查询指定股票、指定报告期的财务指标
func (r *FinaRepo) GetByCodeAndPeriod(tsCode, period string) (*model.FinaIndicator, error) {
	query := fmt.Sprintf(`SELECT %s FROM fina_indicator WHERE ts_code = ? AND end_date = ?`, finaSelectCols)
	var fi model.FinaIndicator
	if err := r.db.Get(&fi, query, tsCode, period); err != nil {
		if isNoRowsErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("查询财务指标失败: %w", err)
	}
	return &fi, nil
}

// GetMaxEndDate 获取已同步的最新报告期(end_date), 用于增量同步判断
// 无数据时返回空字符串
func (r *FinaRepo) GetMaxEndDate() (string, error) {
	var maxDate string
	err := r.db.Get(&maxDate, `SELECT MAX(end_date) FROM fina_indicator`)
	if err != nil {
		if isNoRowsErr(err) {
			return "", nil
		}
		return "", fmt.Errorf("查询最新报告期失败: %w", err)
	}
	return maxDate, nil
}
