package store

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// ScreenRepo 选股域仓储：screen_funnel / screen_watchlist / screen_result 的
// 幂等批量写，以及选股、信号、快照所需的行情读取辅助（全部走读池）。
// 只做 CRUD，不包含任何筛选/打分业务判断（§11.5）。
type ScreenRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// ScreenRepo 返回选股域仓储。
func (s *Store) ScreenRepo() *ScreenRepo {
	return &ScreenRepo{wdb: s.writeDB, rdb: s.readDB}
}

// ===================== 行类型 =====================

// FunnelRow screen_funnel 一行：某级筛选的进出计数与淘汰原因分布（JSON）。
type FunnelRow struct {
	TradeDate   string `db:"trade_date"`
	Stage       int    `db:"stage"`
	StageName   string `db:"stage_name"`
	PassedIn    int    `db:"passed_in"`
	PassedOut   int    `db:"passed_out"`
	Dropped     int    `db:"dropped"`
	DropReasons string `db:"drop_reasons"` // JSON: {"原因": 计数}
	Thresholds  string `db:"thresholds"`   // JSON: 当次筛选参数快照
}

// WatchRow screen_watchlist 一行：候选为 0 时的降级观察名单。
type WatchRow struct {
	TradeDate string  `db:"trade_date"`
	TsCode    string  `db:"ts_code"`
	Rank      int     `db:"rank"`
	Score     float64 `db:"score"`
	Reason    string  `db:"reason"`
}

// ClosePoint 日线序列单点（供动量/波动率等因子批量计算）。
type ClosePoint struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	Close     float64 `db:"close"`    // 前复权收盘（因子计算用，比值无单位）
	VolLot    float64 `db:"vol_lot"`  // 成交量（手）
	RawClose  float64 `db:"raw_close"` // 未复权收盘（分）
}

// ===================== 幂等批量写 =====================

// ReplaceFunnel 全量重写指定交易日的选股漏斗（先删后插，重跑幂等）。
func (r *ScreenRepo) ReplaceFunnel(ctx context.Context, tradeDate string, rows []FunnelRow) error {
	err := WithTx(ctx, r.wdb, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM screen_funnel WHERE trade_date = ?", tradeDate); err != nil {
			return fmt.Errorf("清空选股漏斗 %s 失败: %w", tradeDate, err)
		}
		for _, row := range rows {
			const q = `INSERT INTO screen_funnel (trade_date, stage, stage_name, passed_in, passed_out, dropped, drop_reasons, thresholds)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
			if _, err := tx.ExecContext(ctx, q,
				row.TradeDate, row.Stage, row.StageName, row.PassedIn, row.PassedOut, row.Dropped, row.DropReasons, row.Thresholds,
			); err != nil {
				return fmt.Errorf("写入选股漏斗 %s 第 %d 级失败: %w", tradeDate, row.Stage, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("重写选股漏斗 %s 失败: %w", tradeDate, err)
	}
	return nil
}

// ReplaceWatchlist 全量重写指定交易日的观察名单（重跑幂等）。
func (r *ScreenRepo) ReplaceWatchlist(ctx context.Context, tradeDate string, rows []WatchRow) error {
	err := WithTx(ctx, r.wdb, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM screen_watchlist WHERE trade_date = ?", tradeDate); err != nil {
			return fmt.Errorf("清空观察名单 %s 失败: %w", tradeDate, err)
		}
		for _, row := range rows {
			const q = `INSERT INTO screen_watchlist (trade_date, ts_code, rank, score, reason) VALUES (?, ?, ?, ?, ?)`
			if _, err := tx.ExecContext(ctx, q, row.TradeDate, row.TsCode, row.Rank, row.Score, row.Reason); err != nil {
				return fmt.Errorf("写入观察名单 %s/%s 失败: %w", tradeDate, row.TsCode, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("重写观察名单 %s 失败: %w", tradeDate, err)
	}
	return nil
}

// ReplaceScreenResults 全量重写指定交易日的选股结果（先删后插，重跑幂等）。
func (r *ScreenRepo) ReplaceScreenResults(ctx context.Context, tradeDate string, rows []model.ScreenResult) error {
	err := WithTx(ctx, r.wdb, func(tx *sqlx.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM screen_result WHERE trade_date = ?", tradeDate); err != nil {
			return fmt.Errorf("清空选股结果 %s 失败: %w", tradeDate, err)
		}
		const q = `INSERT INTO screen_result
			(trade_date, ts_code, rank, score, f_momentum, f_quality, f_value, f_lowvol, f_liquidity,
			 close, circ_mv_w, pe_ttm, pb, turnover_rate, reason)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
		for _, sr := range rows {
			if _, err := tx.ExecContext(ctx, q,
				sr.TradeDate, sr.TsCode, sr.Rank, sr.Score, sr.F_Momentum, sr.F_Quality, sr.F_Value, sr.F_LowVol, sr.F_Liquidity,
				int64(sr.Close), sr.CircMvW, sr.PETtm, sr.PB, sr.TurnoverRate, sr.Reason,
			); err != nil {
				return fmt.Errorf("写入选股结果 %s/%s 失败: %w", tradeDate, sr.TsCode, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("重写选股结果 %s 失败: %w", tradeDate, err)
	}
	return nil
}

// ListScreenResults 读取指定交易日选股结果（按 rank 升序）。
func (r *ScreenRepo) ListScreenResults(ctx context.Context, tradeDate string) ([]model.ScreenResult, error) {
	var rows []model.ScreenResult
	const q = `SELECT trade_date, ts_code, rank, score, f_momentum, f_quality, f_value, f_lowvol, f_liquidity,
		close, circ_mv_w, pe_ttm, pb, turnover_rate, reason
		FROM screen_result WHERE trade_date = ? ORDER BY rank`
	if err := r.rdb.SelectContext(ctx, &rows, q, tradeDate); err != nil {
		return nil, fmt.Errorf("读取选股结果 %s 失败: %w", tradeDate, err)
	}
	return rows, nil
}

// ListWatchlist 读取指定交易日观察名单（按 rank 升序）。
func (r *ScreenRepo) ListWatchlist(ctx context.Context, tradeDate string) ([]WatchRow, error) {
	var rows []WatchRow
	const q = `SELECT trade_date, ts_code, rank, score, reason FROM screen_watchlist WHERE trade_date = ? ORDER BY rank`
	if err := r.rdb.SelectContext(ctx, &rows, q, tradeDate); err != nil {
		return nil, fmt.Errorf("读取观察名单 %s 失败: %w", tradeDate, err)
	}
	return rows, nil
}

// ListFunnel 读取指定交易日漏斗（按 stage 升序）。
func (r *ScreenRepo) ListFunnel(ctx context.Context, tradeDate string) ([]FunnelRow, error) {
	var rows []FunnelRow
	const q = `SELECT trade_date, stage, stage_name, passed_in, passed_out, dropped, drop_reasons, thresholds
		FROM screen_funnel WHERE trade_date = ? ORDER BY stage`
	if err := r.rdb.SelectContext(ctx, &rows, q, tradeDate); err != nil {
		return nil, fmt.Errorf("读取选股漏斗 %s 失败: %w", tradeDate, err)
	}
	return rows, nil
}

// ===================== 选股数据读取（读池） =====================

// LiveStocks 读取全部在市股票（list_status='L'）。
func (r *ScreenRepo) LiveStocks(ctx context.Context) ([]model.StockBasic, error) {
	var rows []model.StockBasic
	const q = `SELECT ts_code, symbol, name, market, exchange, industry, list_date, delist_date, is_st, list_status, updated_at
		FROM stock_basic WHERE list_status = 'L' ORDER BY ts_code`
	if err := r.rdb.SelectContext(ctx, &rows, q); err != nil {
		return nil, fmt.Errorf("读取在市股票失败: %w", err)
	}
	return rows, nil
}

// StockNameMap 读取全部股票代码 → 名称映射。
func (r *ScreenRepo) StockNameMap(ctx context.Context) (map[string]string, error) {
	var rows []struct {
		TsCode string `db:"ts_code"`
		Name   string `db:"name"`
	}
	if err := r.rdb.SelectContext(ctx, &rows, "SELECT ts_code, name FROM stock_basic"); err != nil {
		return nil, fmt.Errorf("读取股票名称映射失败: %w", err)
	}
	m := make(map[string]string, len(rows))
	for _, row := range rows {
		m[row.TsCode] = row.Name
	}
	return m, nil
}

// SuspendedMap 读取指定交易日停牌集合。
func (r *ScreenRepo) SuspendedMap(ctx context.Context, tradeDate string) (map[string]bool, error) {
	var codes []string
	if err := r.rdb.SelectContext(ctx, &codes,
		"SELECT DISTINCT ts_code FROM suspend_d WHERE trade_date = ?", tradeDate); err != nil {
		return nil, fmt.Errorf("读取停牌集合 %s 失败: %w", tradeDate, err)
	}
	m := make(map[string]bool, len(codes))
	for _, c := range codes {
		m[c] = true
	}
	return m, nil
}

// BasicAt 读取指定交易日全市场每日指标。
func (r *ScreenRepo) BasicAt(ctx context.Context, tradeDate string) ([]model.DailyBasic, error) {
	var rows []model.DailyBasic
	const q = `SELECT ts_code, trade_date, close, turnover_rate, volume_ratio, pe, pe_ttm, pb, ps_ttm, dv_ratio, total_mv_w, circ_mv_w
		FROM daily_basic WHERE trade_date = ?`
	if err := r.rdb.SelectContext(ctx, &rows, q, tradeDate); err != nil {
		return nil, fmt.Errorf("读取每日指标 %s 失败: %w", tradeDate, err)
	}
	return rows, nil
}

// RecentTradeDates 返回日线中出现过的最近 n 个交易日（降序，含 beforeInclusive 之前）。
func (r *ScreenRepo) RecentTradeDates(ctx context.Context, beforeInclusive string, n int) ([]string, error) {
	var dates []string
	if err := r.rdb.SelectContext(ctx, &dates,
		"SELECT DISTINCT trade_date FROM daily_bar WHERE trade_date <= ? ORDER BY trade_date DESC LIMIT ?",
		beforeInclusive, n); err != nil {
		return nil, fmt.Errorf("读取最近交易日失败: %w", err)
	}
	return dates, nil
}

// BarCloseSeries 读取 fromDate（含）之后的日线序列点（按代码、日期升序），
// 用于动量/低波因子与买入指标（MA、量比）批量计算。
func (r *ScreenRepo) BarCloseSeries(ctx context.Context, fromDate string) ([]ClosePoint, error) {
	var rows []ClosePoint
	const q = `SELECT ts_code, trade_date, close, vol_lot, raw_close FROM daily_bar
		WHERE trade_date >= ? ORDER BY ts_code, trade_date`
	if err := r.rdb.SelectContext(ctx, &rows, q, fromDate); err != nil {
		return nil, fmt.Errorf("读取日线序列（自 %s）失败: %w", fromDate, err)
	}
	return rows, nil
}

// FinaLatestAsOf 读取截至 asOf（按 ann_date point-in-time，无前视）每只股票最新一期财务指标。
func (r *ScreenRepo) FinaLatestAsOf(ctx context.Context, asOf string) (map[string]model.FinaIndicator, error) {
	var rows []model.FinaIndicator
	const q = `SELECT ts_code, end_date, ann_date, eps, roe, roe_dt, grossprofit_margin, netprofit_margin,
		debt_to_assets, netprofit_yoy, or_yoy, bps
		FROM fina_indicator WHERE ann_date <= ? ORDER BY ann_date, end_date`
	if err := r.rdb.SelectContext(ctx, &rows, q, asOf); err != nil {
		return nil, fmt.Errorf("读取财务指标（截至 %s）失败: %w", asOf, err)
	}
	m := make(map[string]model.FinaIndicator, len(rows))
	for _, row := range rows { // 升序遍历，后写覆盖 → 保留最新 ann_date
		m[row.TsCode] = row
	}
	return m, nil
}

// LatestBarAt 读取某标的指定交易日（含）之前最近一根日线；停牌股自然取停牌前收盘。
func (r *ScreenRepo) LatestBarAt(ctx context.Context, tsCode, beforeInclusive string) (model.Bar, error) {
	var b model.Bar
	const q = `SELECT ts_code, trade_date, open, high, low, close, pre_close, pct_chg, vol_lot, amount_k, adj_factor, raw_close
		FROM daily_bar WHERE ts_code = ? AND trade_date <= ? ORDER BY trade_date DESC LIMIT 1`
	if err := r.rdb.GetContext(ctx, &b, q, tsCode, beforeInclusive); err != nil {
		return b, fmt.Errorf("读取最近日线 %s（≤%s）失败: %w", tsCode, beforeInclusive, err)
	}
	return b, nil
}

// IndexAt 读取某指数指定交易日（含）之前最近一根日线（大盘恶化规则用）。
func (r *ScreenRepo) IndexAt(ctx context.Context, tsCode, beforeInclusive string) (model.IndexDaily, error) {
	var d model.IndexDaily
	const q = `SELECT ts_code, trade_date, close, ma20 FROM index_daily
		WHERE ts_code = ? AND trade_date <= ? ORDER BY trade_date DESC LIMIT 1`
	if err := r.rdb.GetContext(ctx, &d, q, tsCode, beforeInclusive); err != nil {
		return d, fmt.Errorf("读取指数日线 %s（≤%s）失败: %w", tsCode, beforeInclusive, err)
	}
	return d, nil
}

// LatestIndexAny 读取库中任一指数的最新日线（优先上证指数；指数数据缺失返回错误由调用方降级）。
func (r *ScreenRepo) LatestIndexAny(ctx context.Context, beforeInclusive string) (model.IndexDaily, error) {
	var d model.IndexDaily
	const q = `SELECT ts_code, trade_date, close, ma20 FROM index_daily
		WHERE trade_date <= ? AND ts_code = '000001.SH'
		ORDER BY trade_date DESC LIMIT 1`
	err := r.rdb.GetContext(ctx, &d, q, beforeInclusive)
	if err == nil {
		return d, nil
	}
	// 无上证指数 → 取任意指数的最新一根
	const qAny = `SELECT ts_code, trade_date, close, ma20 FROM index_daily
		WHERE trade_date <= ? ORDER BY trade_date DESC, ts_code LIMIT 1`
	if err := r.rdb.GetContext(ctx, &d, qAny, beforeInclusive); err != nil {
		return d, fmt.Errorf("读取任意指数日线（≤%s）失败: %w", beforeInclusive, err)
	}
	return d, nil
}

// LimitsAt 读取指定交易日全市场涨跌停价（ts_code → 价格）。
func (r *ScreenRepo) LimitsAt(ctx context.Context, tradeDate string) (map[string]model.PriceLimit, error) {
	var rows []model.PriceLimit
	const q = `SELECT ts_code, trade_date, up_limit, down_limit FROM stk_limit WHERE trade_date = ?`
	if err := r.rdb.SelectContext(ctx, &rows, q, tradeDate); err != nil {
		return nil, fmt.Errorf("读取涨跌停 %s 失败: %w", tradeDate, err)
	}
	m := make(map[string]model.PriceLimit, len(rows))
	for _, row := range rows {
		m[row.TsCode] = row
	}
	return m, nil
}
