package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// MarketRepo 市场域仓储：trade_cal / stock_basic（含估值截面）/ daily_bar，
// 外加停牌集合（落在 config_kv 的 suspend:<日期> 一行，见 SaveSuspended）。
//
// 指数日线与个股日线共用 daily_bar（同形、同保留窗口、代码不重叠）。
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
//
// 只有"开不开市"与"这行是不是前向补齐合成的"两件事。前后交易日按 cal_date 行序现算，
// 不从库里抄镜像（原 pretrade_date/nexttrade_date 两列零读者）。
type CalRow struct {
	CalDate   string `db:"cal_date"`
	IsOpen    bool   `db:"is_open"`
	Synthetic bool   `db:"synthetic"` // 真实日历够不着时按工作日补齐的行
}

// UpsertCal 写入交易日历。
func (r *MarketRepo) UpsertCal(ctx context.Context, c CalRow) error {
	const q = `INSERT INTO trade_cal (cal_date, is_open, synthetic)
		VALUES (?, ?, ?)
		ON CONFLICT(cal_date) DO UPDATE SET
			is_open = excluded.is_open, synthetic = excluded.synthetic`
	if _, err := r.wdb.ExecContext(ctx, q, c.CalDate, boolToInt(c.IsOpen), boolToInt(c.Synthetic)); err != nil {
		return fmt.Errorf("写入交易日历 %s 失败: %w", c.CalDate, err)
	}
	return nil
}

// LoadTradeCal 读取全部交易日历，返回 map[日期]=是否开市（供 market.IsTradeDay 使用）。
func (r *MarketRepo) LoadTradeCal(ctx context.Context) (map[string]bool, error) {
	var rows []struct {
		CalDate string `db:"cal_date"`
		IsOpen  bool   `db:"is_open"`
	}
	if err := r.rdb.SelectContext(ctx, &rows, "SELECT cal_date, is_open FROM trade_cal"); err != nil {
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

// MaxCalDate 返回 trade_cal 中最大的 cal_date（空表返回空串）。
// 用于新鲜度门禁/日历前向补齐的定位起点。
func (r *MarketRepo) MaxCalDate(ctx context.Context) (string, error) {
	var d string
	if err := r.rdb.GetContext(ctx, &d, "SELECT MAX(cal_date) FROM trade_cal"); err != nil {
		return "", fmt.Errorf("读取交易日历最大日期失败: %w", err)
	}
	return d, nil
}

// DeleteSyntheticCal 清空前向补齐生成的合成行（synthetic=1，按工作日造、不含节假日）。
// 补齐前先清后建，保证合成行永远只有"真实日历余量不足"的那一小段，不会逐年堆积。
func (r *MarketRepo) DeleteSyntheticCal(ctx context.Context) (int, error) {
	res, err := r.wdb.ExecContext(ctx, `DELETE FROM trade_cal WHERE synthetic = 1`)
	if err != nil {
		return 0, fmt.Errorf("清理合成日历行失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("统计合成日历行清理条数失败: %w", err)
	}
	return int(n), nil
}

// TrimCal 删除滚动窗 [from,to] 之外的日历行（含上一轮生成的合成行），返回删除条数。
func (r *MarketRepo) TrimCal(ctx context.Context, from, to string) (int, error) {
	res, err := r.wdb.ExecContext(ctx, `DELETE FROM trade_cal WHERE cal_date < ? OR cal_date > ?`, from, to)
	if err != nil {
		return 0, fmt.Errorf("裁剪交易日历 %s~%s 之外行失败: %w", from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("统计交易日历裁剪条数失败: %w", err)
	}
	return int(n), nil
}

// ===================== stock_basic =====================

// UpsertStockBasic 写入股票静态信息（每只票一行当前快照，原地覆盖）。
//
// 只碰静态四列：估值截面由 SaveValuation 单独写，两个接口各有各的更新节奏，
// 谁也不该把对方的值清零。是否 ST 不写列（名称即真相源，见 market.IsSTName）。
func (r *MarketRepo) UpsertStockBasic(ctx context.Context, s model.StockBasic) error {
	const q = `INSERT INTO stock_basic (ts_code, name, industry, list_date, list_status)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(ts_code) DO UPDATE SET
			name=excluded.name, industry=excluded.industry,
			list_date=excluded.list_date, list_status=excluded.list_status`
	if _, err := r.wdb.ExecContext(ctx, q,
		s.TsCode, s.Name, s.Industry, s.ListDate, s.ListStatus,
	); err != nil {
		return fmt.Errorf("写入股票基础 %s 失败: %w", s.TsCode, err)
	}
	return nil
}

// ===================== daily_bar（个股 + 指数）=====================

// UpsertBar 写入前复权日线（ON CONFLICT 覆盖）。指数行 VolLot/RawClose 传 0。
func (r *MarketRepo) UpsertBar(ctx context.Context, b model.Bar) error {
	const q = `INSERT INTO daily_bar (ts_code, trade_date, close, vol_lot, raw_close)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(ts_code, trade_date) DO UPDATE SET
			close=excluded.close, vol_lot=excluded.vol_lot, raw_close=excluded.raw_close`
	if _, err := r.wdb.ExecContext(ctx, q,
		b.TsCode, b.TradeDate, int64(b.Close), b.VolLot, int64(b.RawClose),
	); err != nil {
		return fmt.Errorf("写入日线 %s/%s 失败: %w", b.TsCode, b.TradeDate, err)
	}
	return nil
}

// ===================== 估值截面 / 停牌集合 =====================

// SaveValuation 把某交易日的估值截面盖到 stock_basic 的估值列上（一次同步一个整批）。
//
// 只 UPDATE 不 INSERT：不在 stock_basic 里的代码不属于可投 universe（未上市/已摘牌），
// 给它建一行没有读者。整个截面一个事务，避免"一半新一半旧"的截面被选股读到。
func (r *MarketRepo) SaveValuation(ctx context.Context, tradeDate string, rows []model.Valuation) error {
	const q = `UPDATE stock_basic SET val_date=?, turnover_rate=?, pe_ttm=?, pb=?, circ_mv_w=?
		WHERE ts_code=?`
	return WithTx(ctx, r.wdb, func(tx *sqlx.Tx) error {
		st, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("准备估值截面写入失败: %w", err)
		}
		defer func() { _ = st.Close() }()
		for _, v := range rows {
			if _, err := st.ExecContext(ctx, tradeDate, v.TurnoverRate, v.PETtm, v.PB, v.CircMvW, v.TsCode); err != nil {
				return fmt.Errorf("写入估值截面 %s/%s 失败: %w", v.TsCode, tradeDate, err)
			}
		}
		return nil
	})
}

// suspendKey 当日停牌集合在 config_kv 里的键。日期为 YYYYMMDD，字典序即时间序，
// 保留策略因此能按键区间清理（见 store.RetentionRule.KeyPrefix）。
func suspendKey(tradeDate string) string { return "suspend:" + tradeDate }

// SaveSuspended 覆盖写入指定交易日的停牌代码集合。
//
// 原 suspend_d 一码一行、一次同步要写上百行，而两个读者都按"当天整批"读，
// 故收成一天一行。空集合同样覆盖：不覆盖的话，上一次同步留下的更长列表会在
// 重跑后继续把本来有行情的票豁免掉。
func (r *MarketRepo) SaveSuspended(ctx context.Context, tradeDate string, codes []string) error {
	cr := ConfigRepo{db: r.wdb}
	if err := cr.Set(ctx, suspendKey(tradeDate), strings.Join(codes, ",")); err != nil {
		return fmt.Errorf("写入 %s 停牌集合失败: %w", tradeDate, err)
	}
	return nil
}

// ===================== 读取辅助（新鲜度门禁 / 自检）=====================

// CountBar 返回指定交易日 daily_bar 行数。
func (r *MarketRepo) CountBar(ctx context.Context, tradeDate string) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM daily_bar WHERE trade_date = ?", tradeDate); err != nil {
		return 0, fmt.Errorf("统计日线行数失败: %w", err)
	}
	return n, nil
}

// CountValuation 返回估值截面属于指定交易日的股票数（自检"每日指标是否当日"）。
func (r *MarketRepo) CountValuation(ctx context.Context, tradeDate string) (int, error) {
	var n int
	if err := r.rdb.GetContext(ctx, &n, "SELECT COUNT(*) FROM stock_basic WHERE val_date = ?", tradeDate); err != nil {
		return 0, fmt.Errorf("统计估值截面行数失败: %w", err)
	}
	return n, nil
}

// CountIndexBar 返回指定指数在指定交易日的日线条数（指数与个股共用 daily_bar）。
func (r *MarketRepo) CountIndexBar(ctx context.Context, tsCode, tradeDate string) (int, error) {
	var n int
	q := `SELECT COUNT(*) FROM daily_bar WHERE ts_code = ? AND trade_date = ?`
	if err := r.rdb.GetContext(ctx, &n, q, tsCode, tradeDate); err != nil {
		return 0, fmt.Errorf("统计指数日线行数失败: %w", err)
	}
	return n, nil
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

// SuspendedCodes 返回指定交易日停牌的代码集合（config_kv 的 suspend:<日期> 一行）。
// 停牌股本就无日线，若计入覆盖缺口会误判"数据不新鲜"，导致门禁永久失败。
// 没有这一行（尚未同步过停牌）返回空集合：不排除任何票，与"当日零停牌"同结果。
func (r *MarketRepo) SuspendedCodes(ctx context.Context, tradeDate string) ([]string, error) {
	var v string
	err := r.rdb.GetContext(ctx, &v, "SELECT value FROM config_kv WHERE key = ?", suspendKey(tradeDate))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取停牌集合 %s 失败: %w", tradeDate, err)
	}
	if v == "" {
		return nil, nil
	}
	return strings.Split(v, ","), nil
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
