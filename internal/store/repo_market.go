package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// MarketRepo 市场域仓储：trade_cal / stock_basic / daily_bar / daily_basic /
// stk_limit / suspend_d / adj_factor / index_daily / moneyflow。
type MarketRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// MarketRepo 返回市场域仓储。
func (s *Store) MarketRepo() *MarketRepo {
	return &MarketRepo{wdb: s.writeDB, rdb: s.readDB}
}

// ===================== trade_cal =====================

// CalRow 交易日历行（独立类型避免与 model 耦合过深）。
type CalRow struct {
	CalDate       string `db:"cal_date"`
	IsOpen        bool   `db:"is_open"`
	PreTradeDate  string `db:"pretrade_date"`
	NextTradeDate string `db:"nexttrade_date"`
	Exchange      string `db:"exchange"`
}

// UpsertCal 写入交易日历。
func (r *MarketRepo) UpsertCal(ctx context.Context, c CalRow) error {
	const q = `INSERT INTO trade_cal (cal_date, is_open, pretrade_date, nexttrade_date, exchange)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(cal_date) DO UPDATE SET
			is_open = excluded.is_open,
			pretrade_date = excluded.pretrade_date,
			nexttrade_date = excluded.nexttrade_date,
			exchange = excluded.exchange`
	if _, err := r.wdb.ExecContext(ctx, q, c.CalDate, c.IsOpen, c.PreTradeDate, c.NextTradeDate, c.Exchange); err != nil {
		return fmt.Errorf("写入交易日历 %s 失败: %w", c.CalDate, err)
	}
	return nil
}

// LoadTradeCal 读取全部交易日历，返回 map[日期]=是否开市（供 market.IsTradeDay 使用）。
func (r *MarketRepo) LoadTradeCal(ctx context.Context) (map[string]bool, error) {
	rows := []CalRow{}
	if err := r.rdb.SelectContext(ctx, &rows, "SELECT cal_date, is_open, pretrade_date, nexttrade_date, exchange FROM trade_cal"); err != nil {
		return nil, fmt.Errorf("读取交易日历失败: %w", err)
	}
	m := make(map[string]bool, len(rows))
	for _, c := range rows {
		m[c.CalDate] = c.IsOpen
	}
	return m, nil
}

// TradeDateList 返回升序排列的交易日列表（供 market.NextTradeDay/PrevTradeDay 使用）。
func (r *MarketRepo) TradeDateList(ctx context.Context) ([]string, error) {
	var dates []string
	if err := r.rdb.SelectContext(ctx, &dates, "SELECT cal_date FROM trade_cal WHERE is_open = 1 ORDER BY cal_date"); err != nil {
		return nil, fmt.Errorf("读取交易日列表失败: %w", err)
	}
	return dates, nil
}

// ===================== stock_basic =====================

// UpsertStockBasic 写入股票基础信息。
func (r *MarketRepo) UpsertStockBasic(ctx context.Context, s model.StockBasic) error {
	const q = `INSERT INTO stock_basic (ts_code, symbol, name, market, exchange, industry, list_date, delist_date, is_st, list_status, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code) DO UPDATE SET
			symbol=excluded.symbol, name=excluded.name, market=excluded.market, exchange=excluded.exchange,
			industry=excluded.industry, list_date=excluded.list_date, delist_date=excluded.delist_date,
			is_st=excluded.is_st, list_status=excluded.list_status, updated_at=excluded.updated_at`
	if _, err := r.wdb.ExecContext(ctx, q,
		s.TsCode, s.Symbol, s.Name, s.Market, s.Exchange, s.Industry, s.ListDate, s.DelistDate, s.IsST, s.ListStatus, s.UpdatedAt,
	); err != nil {
		return fmt.Errorf("写入股票基础 %s 失败: %w", s.TsCode, err)
	}
	return nil
}

// ===================== daily_bar =====================

// UpsertBar 写入前复权日线（ON CONFLICT 覆盖）。
func (r *MarketRepo) UpsertBar(ctx context.Context, b model.Bar) error {
	const q = `INSERT INTO daily_bar (ts_code, trade_date, open, high, low, close, pre_close, pct_chg, vol_lot, amount_k, adj_factor, raw_close)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET
			open=excluded.open, high=excluded.high, low=excluded.low, close=excluded.close,
			pre_close=excluded.pre_close, pct_chg=excluded.pct_chg, vol_lot=excluded.vol_lot,
			amount_k=excluded.amount_k, adj_factor=excluded.adj_factor, raw_close=excluded.raw_close`
	if _, err := r.wdb.ExecContext(ctx, q,
		b.TsCode, b.TradeDate, int64(b.Open), int64(b.High), int64(b.Low), int64(b.Close), int64(b.PreClose),
		b.PctChg, b.VolLot, b.AmountK, b.AdjFactor, int64(b.RawClose),
	); err != nil {
		return fmt.Errorf("写入日线 %s/%s 失败: %w", b.TsCode, b.TradeDate, err)
	}
	return nil
}

// Bar 读取指定标的指定交易日的前复权日线。
func (r *MarketRepo) Bar(ctx context.Context, tsCode, tradeDate string) (model.Bar, error) {
	var b model.Bar
	err := r.rdb.GetContext(ctx, &b,
		`SELECT ts_code, trade_date, open, high, low, close, pre_close, pct_chg, vol_lot, amount_k, adj_factor, raw_close
		 FROM daily_bar WHERE ts_code = ? AND trade_date = ?`, tsCode, tradeDate)
	if err != nil {
		return b, fmt.Errorf("读取日线 %s/%s 失败: %w", tsCode, tradeDate, err)
	}
	return b, nil
}

// ===================== daily_basic =====================

// UpsertDailyBasic 写入每日指标。
func (r *MarketRepo) UpsertDailyBasic(ctx context.Context, d model.DailyBasic) error {
	const q = `INSERT INTO daily_basic (ts_code, trade_date, close, turnover_rate, volume_ratio, pe, pe_ttm, pb, ps_ttm, dv_ratio, total_mv_w, circ_mv_w)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET
			close=excluded.close, turnover_rate=excluded.turnover_rate, volume_ratio=excluded.volume_ratio,
			pe=excluded.pe, pe_ttm=excluded.pe_ttm, pb=excluded.pb, ps_ttm=excluded.ps_ttm,
			dv_ratio=excluded.dv_ratio, total_mv_w=excluded.total_mv_w, circ_mv_w=excluded.circ_mv_w`
	if _, err := r.wdb.ExecContext(ctx, q,
		d.TsCode, d.TradeDate, int64(d.Close), d.TurnoverRate, d.VolumeRatio, d.PE, d.PETtm, d.PB, d.PsTtm, d.DvRatio, d.TotalMvW, d.CircMvW,
	); err != nil {
		return fmt.Errorf("写入每日指标 %s/%s 失败: %w", d.TsCode, d.TradeDate, err)
	}
	return nil
}

// ===================== stk_limit / suspend_d / adj_factor / index_daily / moneyflow =====================

// UpsertLimit 写入涨跌停价。
func (r *MarketRepo) UpsertLimit(ctx context.Context, p model.PriceLimit) error {
	const q = `INSERT INTO stk_limit (ts_code, trade_date, up_limit, down_limit)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET up_limit=excluded.up_limit, down_limit=excluded.down_limit`
	if _, err := r.wdb.ExecContext(ctx, q, p.TsCode, p.TradeDate, int64(p.UpLimit), int64(p.DownLimit)); err != nil {
		return fmt.Errorf("写入涨跌停 %s/%s 失败: %w", p.TsCode, p.TradeDate, err)
	}
	return nil
}

// UpsertSuspend 写入停牌信息。
func (r *MarketRepo) UpsertSuspend(ctx context.Context, s model.Suspend) error {
	const q = `INSERT INTO suspend_d (ts_code, trade_date, suspend_type, suspend_timing)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET suspend_type=excluded.suspend_type, suspend_timing=excluded.suspend_timing`
	if _, err := r.wdb.ExecContext(ctx, q, s.TsCode, s.TradeDate, s.SuspendType, s.SuspendTiming); err != nil {
		return fmt.Errorf("写入停牌 %s/%s 失败: %w", s.TsCode, s.TradeDate, err)
	}
	return nil
}

// UpsertAdjFactor 写入复权因子。
func (r *MarketRepo) UpsertAdjFactor(ctx context.Context, tsCode, tradeDate string, factor float64) error {
	const q = `INSERT INTO adj_factor (ts_code, trade_date, adj_factor)
		VALUES (?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET adj_factor=excluded.adj_factor`
	if _, err := r.wdb.ExecContext(ctx, q, tsCode, tradeDate, factor); err != nil {
		return fmt.Errorf("写入复权因子 %s/%s 失败: %w", tsCode, tradeDate, err)
	}
	return nil
}

// UpsertIndexDaily 写入大盘指数日线。
func (r *MarketRepo) UpsertIndexDaily(ctx context.Context, d model.IndexDaily) error {
	const q = `INSERT INTO index_daily (ts_code, trade_date, close, ma20)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET close=excluded.close, ma20=excluded.ma20`
	if _, err := r.wdb.ExecContext(ctx, q, d.TsCode, d.TradeDate, int64(d.Close), d.MA20); err != nil {
		return fmt.Errorf("写入指数日线 %s/%s 失败: %w", d.TsCode, d.TradeDate, err)
	}
	return nil
}

// UpsertMoneyFlow 写入个股资金流。
func (r *MarketRepo) UpsertMoneyFlow(ctx context.Context, m model.MoneyFlow) error {
	const q = `INSERT INTO moneyflow (ts_code, trade_date, buy_elg_amount, sell_elg_amount, net_mf_amount)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET
			buy_elg_amount=excluded.buy_elg_amount, sell_elg_amount=excluded.sell_elg_amount, net_mf_amount=excluded.net_mf_amount`
	if _, err := r.wdb.ExecContext(ctx, q, m.TsCode, m.TradeDate, m.BuyElgAmount, m.SellElgAmount, m.NetMfAmount); err != nil {
		return fmt.Errorf("写入资金流 %s/%s 失败: %w", m.TsCode, m.TradeDate, err)
	}
	return nil
}

// ===================== 读取辅助（Batch 2 新鲜度门禁 / qfq 计算）=====================

// MaxBarDate 返回 daily_bar 中最大的 trade_date（空表返回空串）。
// CountFutureTradeDays 统计 date 之后（不含当日）的交易日数（自检/日历续拉判定）。
func (r *MarketRepo) CountFutureTradeDays(ctx context.Context, date string) (int, error) {
	var n int
	err := r.rdb.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM trade_cal WHERE is_open=1 AND cal_date > ?`, date)
	if err != nil {
		return 0, fmt.Errorf("统计未来交易日失败: %w", err)
	}
	return n, nil
}

func (r *MarketRepo) MaxBarDate(ctx context.Context) (string, error) {
	var d string
	if err := r.rdb.GetContext(ctx, &d, "SELECT MAX(trade_date) FROM daily_bar"); err != nil {
		return "", fmt.Errorf("读取日线最大日期失败: %w", err)
	}
	return d, nil
}

// MaxCalDate 返回 trade_cal 中最大的 cal_date（空表返回空串）。
// 用于新鲜度门禁/日历前向补齐的定位起点。
func (r *MarketRepo) MaxCalDate(ctx context.Context) (string, error) {
	var d string
	if err := r.rdb.GetContext(ctx, &d, "SELECT MAX(cal_date) FROM trade_cal"); err != nil {
		return "", fmt.Errorf("读取交易日历最大日期失败: %w", err)
	}
	return d, nil
}

// CountBar 返回指定交易日 daily_bar 行数。
func (r *MarketRepo) CountBar(ctx context.Context, tradeDate string) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM daily_bar WHERE trade_date = ?", tradeDate); err != nil {
		return 0, fmt.Errorf("统计日线行数失败: %w", err)
	}
	return n, nil
}

// CountDailyBasic 返回指定交易日 daily_basic 行数。
func (r *MarketRepo) CountDailyBasic(ctx context.Context, tradeDate string) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM daily_basic WHERE trade_date = ?", tradeDate); err != nil {
		return 0, fmt.Errorf("统计每日指标行数失败: %w", err)
	}
	return n, nil
}

// CountLimit 返回指定交易日 stk_limit 行数。
func (r *MarketRepo) CountLimit(ctx context.Context, tradeDate string) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM stk_limit WHERE trade_date = ?", tradeDate); err != nil {
		return 0, fmt.Errorf("统计涨跌停行数失败: %w", err)
	}
	return n, nil
}

// CountIndexDaily 返回指定标的指定交易日 index_daily 行数。
func (r *MarketRepo) CountIndexDaily(ctx context.Context, tsCode, tradeDate string) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM index_daily WHERE ts_code = ? AND trade_date = ?", tsCode, tradeDate); err != nil {
		return 0, fmt.Errorf("统计指数日线行数失败: %w", err)
	}
	return n, nil
}

// CountIndexDailyAll 返回 index_daily 全表行数（用于判断指数数据是否从未同步）。
func (r *MarketRepo) CountIndexDailyAll(ctx context.Context) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM index_daily"); err != nil {
		return 0, fmt.Errorf("统计指数日线总行数失败: %w", err)
	}
	return n, nil
}

// MaxAdjFactorDate 返回 adj_factor 中最大的 trade_date（空表返回空串）。
// 该日期即前复权（qfq）的归一化参考日（base_adj 来源）。
func (r *MarketRepo) MaxAdjFactorDate(ctx context.Context) (string, error) {
	var d string
	if err := r.rdb.GetContext(ctx, &d, "SELECT MAX(trade_date) FROM adj_factor"); err != nil {
		return "", fmt.Errorf("读取复权因子最大日期失败: %w", err)
	}
	return d, nil
}

// AdjFactorAt 返回指定交易日全部股票的 adj_factor（ts_code -> factor）。
func (r *MarketRepo) AdjFactorAt(ctx context.Context, tradeDate string) (map[string]float64, error) {
	rows := []struct {
		TsCode    string  `db:"ts_code"`
		AdjFactor float64 `db:"adj_factor"`
	}{}
	if err := r.rdb.SelectContext(ctx, &rows, "SELECT ts_code, adj_factor FROM adj_factor WHERE trade_date = ?", tradeDate); err != nil {
		return nil, fmt.Errorf("读取复权因子失败: %w", err)
	}
	m := make(map[string]float64, len(rows))
	for _, row := range rows {
		m[row.TsCode] = row.AdjFactor
	}
	return m, nil
}

// CandidateCodes 返回可投资候选池（stock_basic 中 list_status='L' 的全部 ts_code，升序）。
// 用于新鲜度门禁 #7 的覆盖检查。
func (r *MarketRepo) CandidateCodes(ctx context.Context) ([]string, error) {
	var codes []string
	if err := r.rdb.SelectContext(ctx, &codes,
		"SELECT ts_code FROM stock_basic WHERE list_status = 'L' ORDER BY ts_code"); err != nil {
		return nil, fmt.Errorf("读取候选池失败: %w", err)
	}
	return codes, nil
}

// SuspendedCodes 返回指定交易日停牌的股票代码（升序），供新鲜度门禁覆盖检查排除。
// 停牌股本就无日线，若计入覆盖缺口会误判"数据不新鲜"，导致门禁永久失败。
func (r *MarketRepo) SuspendedCodes(ctx context.Context, tradeDate string) ([]string, error) {
	var codes []string
	if err := r.rdb.SelectContext(ctx, &codes,
		"SELECT DISTINCT ts_code FROM suspend_d WHERE trade_date = ? ORDER BY ts_code", tradeDate); err != nil {
		return nil, fmt.Errorf("读取停牌列表失败: %w", err)
	}
	return codes, nil
}

// AllStockCodes 返回 stock_basic 全部 ts_code（升序），供财务慢路径遍历。
func (r *MarketRepo) AllStockCodes(ctx context.Context) ([]string, error) {
	var codes []string
	if err := r.rdb.SelectContext(ctx, &codes, "SELECT ts_code FROM stock_basic ORDER BY ts_code"); err != nil {
		return nil, fmt.Errorf("读取股票列表失败: %w", err)
	}
	return codes, nil
}

// BarCoverage 返回 codes 中在指定交易日有日线覆盖的数量（codes 过大时分片，避免 SQL 变量上限）。
func (r *MarketRepo) BarCoverage(ctx context.Context, tradeDate string, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	const chunk = 500
	total := 0
	for start := 0; start < len(codes); start += chunk {
		end := start + chunk
		if end > len(codes) {
			end = len(codes)
		}
		batch := codes[start:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		args = append(args, tradeDate)
		for i, c := range batch {
			placeholders[i] = "?"
			args = append(args, c)
		}
		var n int
		q := "SELECT COUNT(DISTINCT ts_code) FROM daily_bar WHERE trade_date = ? AND ts_code IN (" +
			strings.Join(placeholders, ",") + ")"
		if err := r.rdb.GetContext(ctx, &n, q, args...); err != nil {
			return total, fmt.Errorf("统计日线覆盖失败: %w", err)
		}
		total += n
	}
	return total, nil
}
