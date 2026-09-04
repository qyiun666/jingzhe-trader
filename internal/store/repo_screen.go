package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// ScreenRepo 选股只读仓储：因子窗口日线、在市股票当前快照（含估值）、指数读取辅助（全部走读池）。
//
// 选股过程（漏斗每级进出数、候选名单、降级观察名单）不落库——一条流水线内内存传参，
// 计数只写日志，唯一落库的结果是 order_ticket。
// 只做 CRUD，不包含任何筛选/打分业务判断（§11.5）。
type ScreenRepo struct {
	rdb *sqlx.DB
}

// ScreenRepo 返回选股只读仓储。
func (s *Store) ScreenRepo() *ScreenRepo {
	return &ScreenRepo{rdb: s.readDB}
}

// ClosePoint 日线序列单点（供动量/波动率等因子与"一手价"现算）。
type ClosePoint struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	Close     float64 `db:"close"`     // 前复权收盘（因子计算用，比值无单位）
	VolLot    float64 `db:"vol_lot"`   // 成交量（手）
	RawClose  float64 `db:"raw_close"` // 未复权收盘（分）
}

// ===================== 选股数据读取（读池） =====================

// LiveStocks 读取全部在市股票（list_status='L'）的当前快照：静态属性 + 估值截面。
//
// 一次读出即选股的全部横截面输入，不再按日期另查估值表。
func (r *ScreenRepo) LiveStocks(ctx context.Context) ([]model.StockBasic, error) {
	var rows []model.StockBasic
	// 估值五列在首次同步前是 NULL（新股/未跑到那天），COALESCE 成零值再扫，
	// 调用方按"ValDate 不等于选股日"处理，不当成有截面。
	const q = `SELECT ts_code, name, COALESCE(industry,'') AS industry,
		COALESCE(list_date,'') AS list_date, COALESCE(list_status,'') AS list_status,
		COALESCE(val_date,'') AS val_date, COALESCE(turnover_rate,0) AS turnover_rate,
		COALESCE(pe_ttm,0) AS pe_ttm, COALESCE(pb,0) AS pb, COALESCE(circ_mv_w,0) AS circ_mv_w
		FROM stock_basic WHERE list_status = 'L' ORDER BY ts_code`
	if err := r.rdb.SelectContext(ctx, &rows, q); err != nil {
		return nil, fmt.Errorf("读取在市股票失败: %w", err)
	}
	return rows, nil
}

// StockName 读取单只股票名称（卖出决策补名称用）。
func (r *ScreenRepo) StockName(ctx context.Context, tsCode string) (string, error) {
	var name string
	if err := r.rdb.GetContext(ctx, &name, "SELECT name FROM stock_basic WHERE ts_code = ?", tsCode); err != nil {
		return "", fmt.Errorf("读取 %s 名称失败: %w", tsCode, err)
	}
	return name, nil
}

// WindowDates 返回截至 beforeInclusive（含）的最近 n 个交易日，升序。
//
// 以交易日历为准而不是 daily_bar 出现过的日期：后者一旦混入历史残留行（库中曾留有
// 1991 年每日 ~10 行），"最近 20 个交易日"会取到 35 年前的日期，把因子序列变成跨
// 35 年的涨跌幅。
func (r *ScreenRepo) WindowDates(ctx context.Context, beforeInclusive string, n int) ([]string, error) {
	var desc []string
	const q = `SELECT cal_date FROM trade_cal WHERE is_open = 1 AND cal_date <= ?
		ORDER BY cal_date DESC LIMIT ?`
	if err := r.rdb.SelectContext(ctx, &desc, q, beforeInclusive, n); err != nil {
		return nil, fmt.Errorf("读取 %s 最近 %d 个交易日失败: %w", beforeInclusive, n, err)
	}
	dates := make([]string, len(desc))
	for i, d := range desc {
		dates[len(desc)-1-i] = d
	}
	return dates, nil
}

// WindowBarGaps 返回窗口内有日线缺失的交易日（数据缺口判定，全部存在时为空）。
func (r *ScreenRepo) WindowBarGaps(ctx context.Context, dates []string) ([]string, error) {
	if len(dates) == 0 {
		return nil, nil
	}
	var have []string
	q := fmt.Sprintf(`SELECT DISTINCT trade_date FROM daily_bar WHERE trade_date IN (%s)`, placeholders(len(dates)))
	args := dateArgs(dates)
	if err := r.rdb.SelectContext(ctx, &have, q, args...); err != nil {
		return nil, fmt.Errorf("统计窗口日线覆盖失败: %w", err)
	}
	set := make(map[string]bool, len(have))
	for _, d := range have {
		set[d] = true
	}
	var gaps []string
	for _, d := range dates {
		if !set[d] {
			gaps = append(gaps, d)
		}
	}
	return gaps, nil
}

// BarCloseSeries 读取指定交易日窗口的日线序列点（按代码、日期升序），
// 用于动量/低波因子与买入指标（MA、量比）批量计算。日期集合来自 WindowDates，
// 因此读取范围严格等于因子窗口，不受历史残留行影响。
func (r *ScreenRepo) BarCloseSeries(ctx context.Context, dates []string) ([]ClosePoint, error) {
	if len(dates) == 0 {
		return nil, nil
	}
	var rows []ClosePoint
	q := fmt.Sprintf(`SELECT ts_code, trade_date, close, vol_lot, raw_close FROM daily_bar
		WHERE trade_date IN (%s) ORDER BY ts_code, trade_date`, placeholders(len(dates)))
	if err := r.rdb.SelectContext(ctx, &rows, q, dateArgs(dates)...); err != nil {
		return nil, fmt.Errorf("读取日线序列（%d 个交易日）失败: %w", len(dates), err)
	}
	return rows, nil
}

// placeholders 生成 n 个 `?` 逗号串（日期集合固定为内部读法，无外部输入拼接风险）。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func dateArgs(dates []string) []interface{} {
	args := make([]interface{}, len(dates))
	for i, d := range dates {
		args[i] = d
	}
	return args
}

// LatestBarAt 读取某标的指定交易日（含）之前最近一根日线；停牌股自然取停牌前收盘。
func (r *ScreenRepo) LatestBarAt(ctx context.Context, tsCode, beforeInclusive string) (model.Bar, error) {
	var b model.Bar
	const q = `SELECT ts_code, trade_date, close, vol_lot, raw_close
		FROM daily_bar WHERE ts_code = ? AND trade_date <= ? ORDER BY trade_date DESC LIMIT 1`
	if err := r.rdb.GetContext(ctx, &b, q, tsCode, beforeInclusive); err != nil {
		return b, fmt.Errorf("读取最近日线 %s（≤%s）失败: %w", tsCode, beforeInclusive, err)
	}
	return b, nil
}

// IndexQuote 指数行情读取结果：收盘价 + 现算 MA20（分）。
// 指数与个股共用 daily_bar，指数的 vol_lot/raw_close 列为 0。
type IndexQuote struct {
	TsCode    string    `db:"ts_code"`
	TradeDate string    `db:"trade_date"`
	Close     model.Fen `db:"close"`
	MA20      model.Fen `db:"ma20"`
}

// indexColumns 指数读取列：MA20 由最近 20 个交易日的收盘现算（分）。
// 不足 20 根时返回 0，调用方据此判"均线不可算"而不是拿部分均值当真值。
const indexColumns = `ts_code, trade_date, close,
	(SELECT CASE WHEN COUNT(*) = 20 THEN CAST(AVG(x.close) AS INTEGER) ELSE 0 END
	   FROM (SELECT close FROM daily_bar i2
	          WHERE i2.ts_code = i1.ts_code AND i2.trade_date <= i1.trade_date
	          ORDER BY i2.trade_date DESC LIMIT 20) x) AS ma20`

// MarketIndex 大盘门槛所用的指数：沪深300。新鲜度门禁检查的也是这一根，
// 两处必须共用一个常量，否则门禁放行的是一个指数、买入闸门看的是另一个。
const MarketIndex = "000300.SH"

// LatestMarketIndex 读取大盘指数截至 beforeInclusive 的最后一根日线。
//
// 只认 MarketIndex，没有"退而取任意指数"的余地：指数与个股共用 daily_bar，
// 早先那条不加 ts_code 过滤的兜底查询读到的是某只股票的收盘价，却被当成大盘。
func (r *ScreenRepo) LatestMarketIndex(ctx context.Context, beforeInclusive string) (IndexQuote, error) {
	var d IndexQuote
	q := `SELECT ` + indexColumns + ` FROM daily_bar i1
		WHERE i1.ts_code = ? AND i1.trade_date <= ? ORDER BY i1.trade_date DESC LIMIT 1`
	if err := r.rdb.GetContext(ctx, &d, q, MarketIndex, beforeInclusive); err != nil {
		return d, fmt.Errorf("读取大盘指数 %s 日线（≤%s）失败: %w", MarketIndex, beforeInclusive, err)
	}
	return d, nil
}
