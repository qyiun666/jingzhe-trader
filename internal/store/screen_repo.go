package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ScreenResult 选股结果 (定义在 store 包, 避免与 screener 包循环依赖)
type ScreenResult struct {
	TsCode       string  `json:"ts_code" db:"ts_code"`
	Name         string  `json:"name" db:"name"`
	TradeDate    string  `json:"trade_date" db:"trade_date"`
	Close        float64 `json:"close" db:"close"`
	PctChg       float64 `json:"pct_chg" db:"pct_chg"`
	TurnoverRate float64 `json:"turnover_rate" db:"turnover_rate"`
	VolumeRatio  float64 `json:"volume_ratio" db:"volume_ratio"`
	PE           float64 `json:"pe" db:"pe"`
	PE_TTM       float64 `json:"pe_ttm" db:"pe_ttm"`
	PB           float64 `json:"pb" db:"pb"`
	CircMV       float64 `json:"circ_mv" db:"circ_mv"` // 流通市值(万元)
	Score        float64 `json:"score" db:"score"`
	Reason       string  `json:"reason" db:"reason"`
}

// ScreenRepo 选股结果存储
type ScreenRepo struct {
	db *sqlx.DB
}

// NewScreenRepo 创建选股结果仓库
func NewScreenRepo(db *sqlx.DB) *ScreenRepo {
	return &ScreenRepo{db: db}
}

const (
	screenResultInsertSQL = `INSERT OR REPLACE INTO screen_result
		(ts_code, name, trade_date, close, pct_chg, turnover_rate, volume_ratio, pe, pe_ttm, pb, circ_mv, score, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	screenResultSelectCols = `ts_code, name, trade_date, close, pct_chg, turnover_rate, volume_ratio, pe, pe_ttm, pb, circ_mv, score, reason`
)

// SaveResults 保存选股结果 (先删旧的再插新的)
// 空结果时不执行删除, 保留上一日数据供 API 回查与信号新鲜度判断 (避免静默回退过期股票池)
func (r *ScreenRepo) SaveResults(date string, results []ScreenResult) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := r.db.Beginx()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	// 删除当日旧结果
	if _, err := tx.Exec("DELETE FROM screen_result WHERE trade_date = ?", date); err != nil {
		return fmt.Errorf("删除旧选股结果失败: %w", err)
	}

	// 插入新结果
	stmt, err := tx.Preparex(screenResultInsertSQL)
	if err != nil {
		return fmt.Errorf("预编译插入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, res := range results {
		if _, err := stmt.Exec(
			res.TsCode, res.Name, res.TradeDate, res.Close, res.PctChg,
			res.TurnoverRate, res.VolumeRatio, res.PE, res.PE_TTM, res.PB,
			res.CircMV, res.Score, res.Reason,
		); err != nil {
			return fmt.Errorf("插入选股结果失败(ts_code=%s): %w", res.TsCode, err)
		}
	}

	return tx.Commit()
}

// GetByDate 按日期获取选股结果
func (r *ScreenRepo) GetByDate(date string) ([]ScreenResult, error) {
	query := fmt.Sprintf(`SELECT %s FROM screen_result
		WHERE trade_date = ? ORDER BY score DESC`, screenResultSelectCols)
	var results []ScreenResult
	if err := r.db.Select(&results, query, date); err != nil {
		return nil, fmt.Errorf("查询选股结果失败: %w", err)
	}
	return results, nil
}

// GetLatest 获取最新一次选股结果
func (r *ScreenRepo) GetLatest() ([]ScreenResult, error) {
	date, err := maxTableDate(r.db, "screen_result")
	if err != nil || date == "" {
		return nil, nil
	}
	return r.GetByDate(date)
}

// GetLatestDate 获取最新一次选股结果的日期 (无数据返回空字符串)
func (r *ScreenRepo) GetLatestDate() (string, error) {
	return maxTableDate(r.db, "screen_result")
}

// GetScreenedCodes 获取最新选股代码列表 (供策略合并股票池用)
// 仅返回当日选股结果, 过期返回空列表 (避免空候选日静默使用过期股票池)
func (r *ScreenRepo) GetScreenedCodes() ([]string, error) {
	latestDate, err := r.GetLatestDate()
	if err != nil {
		return nil, fmt.Errorf("查询最新选股日期失败: %w", err)
	}
	today := time.Now().Format("20060102")
	if latestDate != today {
		// 选股结果过期, 返回空列表 (不合并过期股票池)
		return []string{}, nil
	}
	var codes []string
	err = r.db.Select(&codes,
		"SELECT ts_code FROM screen_result WHERE trade_date = ? ORDER BY score DESC", latestDate)
	if err != nil {
		return nil, fmt.Errorf("查询选股代码列表失败: %w", err)
	}
	return codes, nil
}
