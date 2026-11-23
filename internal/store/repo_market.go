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

// TradeDateList 返回升序排列的交易日列表（供 market.NextTradeDay 与回补窗口计算使用）。
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

// BarQuote 一根已存在 bar 行的扩展价族（open/high/low/up_limit/down_limit，单位分，
// **未复权真实价**口径）。每列三态：非 nil=有值，nil=unknown（落 NULL）。
//
// 用指针而不是 Fen+一个 valid 布尔位：调用侧不需要记住"0 是什么意思"，写不出 0 价。
type BarQuote struct {
	Open      *model.Fen
	High      *model.Fen
	Low       *model.Fen
	UpLimit   *model.Fen
	DownLimit *model.Fen
}

// fenArg 把 *Fen 转成 SQL 参数（nil → NULL）。显式转换，不依赖 driver 对指针的反射兜底。
func fenArg(v *model.Fen) interface{} {
	if v == nil {
		return nil
	}
	return int64(*v)
}

// UpdateBarQuote 就地补齐 daily_bar **已有行**的 5 列扩展价族，返回是否命中既有行。
//
// UPDATE-only 是这条路径的硬约束（不是风格问题）：这 5 列来自与 daily 不同的接口，
// stk_limit 比 daily 多出的代码（退市、停牌边缘、口径差）一旦走 INSERT ... ON CONFLICT
// 就会在库里造出"只有限价、没有收盘"的行 —— close 是 NOT NULL 无默认，那种插入要么
// 直接报错打断整日同步，要么被逼着给 close 编一个值。两种都不可接受：库里只允许存在
// 真实出过线的 bar。未命中就返回 false，由调用侧计入 unknown。
//
// nil 字段写 NULL 而不是 0：unknown 与"价格为 0"必须能在库里区分（schema.go 口径 1）。
func (r *MarketRepo) UpdateBarQuote(ctx context.Context, tsCode, tradeDate string, q BarQuote) (bool, error) {
	const query = `UPDATE daily_bar
		SET open=?, high=?, low=?, up_limit=?, down_limit=?
		WHERE ts_code=? AND trade_date=?`
	res, err := r.wdb.ExecContext(ctx, query,
		fenArg(q.Open), fenArg(q.High), fenArg(q.Low), fenArg(q.UpLimit), fenArg(q.DownLimit),
		tsCode, tradeDate,
	)
	if err != nil {
		return false, fmt.Errorf("补齐扩展价族 %s/%s 失败: %w", tsCode, tradeDate, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("补齐扩展价族 %s/%s 取影响行数失败: %w", tsCode, tradeDate, err)
	}
	return n > 0, nil
}

// ===================== 封板价格截面（只读，服务阶段1 的收盘封板判定）=====================

// LimitRow 判"收盘封板"所需的一行价格，全部是**未复权真实价**口径。
//
// 为什么把 raw_close 与两列限价捆在同一个类型里、而不是让调用侧自己拼：封板判定就是
// 真实收盘价比真实限价，两个操作数分在两条读法上必然有人拿 close（前复权）去比
// up_limit。20261123 实测这种误用的代价 —— 当日 5,426/5,570 行复权因子≠1，
// `raw_close=up_limit` 命中 152 只，`close=up_limit` 只剩 2 只，等于把封板面读没。
// 本类型**刻意没有 Close 字段**：编译面上就不给误用留路径（组长 20261123 裁定三）。
//
// 两列限价三态：非 nil=有值，nil=库里 NULL=unknown（含"接口给的是无涨跌幅限制的
// 哨兵极值"这类已在写入侧归一的情形，见 dataloader 的 upLimitFen/downLimitFen）。
type LimitRow struct {
	TsCode    string     `db:"ts_code"`
	RawClose  model.Fen  `db:"raw_close"`
	UpLimit   *model.Fen `db:"up_limit"`
	DownLimit *model.Fen `db:"down_limit"`
}

// LimitRowsFor 取指定交易日的封板价格截面，返回**缺失键 map**：库里当日没有该
// (ts_code, trade_date) 行 ⇒ 代码根本不出现在 map 里。
//
// 用「缺失键」而不是「值为零值的 map」：零值的 LimitRow 带着 RawClose=0，看上去像
// "今天收在 0.00 元"，而事实是"今天这根线不存在"（停牌、退市、或还没同步）。让
// 不存在可判只需要 `v, ok := m[code]`，让零值不可判要在每个消费点补一次 `RawClose>0`
// —— 后者一定会漏。store 只给数据不给判定，封板语义归 signal 侧的适配器。
//
// 指数的 5 列扩展价族永久 NULL（schema.go 口径 3），因此指数行会以"有 RawClose、
// 两列限价 nil"的形态出现在 map 里，调用侧不必特殊处理。
func (r *MarketRepo) LimitRowsFor(ctx context.Context, tradeDate string) (map[string]LimitRow, error) {
	var scanned []struct {
		TsCode    string        `db:"ts_code"`
		RawClose  model.Fen     `db:"raw_close"`
		UpLimit   sql.NullInt64 `db:"up_limit"`
		DownLimit sql.NullInt64 `db:"down_limit"`
	}
	// 只取四列：走 idx_bar_date，且把 close 挡在 SELECT 之外（见 LimitRow 注释）。
	const query = `SELECT ts_code, raw_close, up_limit, down_limit FROM daily_bar
		WHERE trade_date = ?`
	if err := r.rdb.SelectContext(ctx, &scanned, query, tradeDate); err != nil {
		return nil, fmt.Errorf("读取 %s 封板价格截面失败: %w", tradeDate, err)
	}
	out := make(map[string]LimitRow, len(scanned))
	for _, s := range scanned {
		row := LimitRow{TsCode: s.TsCode, RawClose: s.RawClose}
		if s.UpLimit.Valid {
			f := model.Fen(s.UpLimit.Int64)
			row.UpLimit = &f
		}
		if s.DownLimit.Valid {
			f := model.Fen(s.DownLimit.Int64)
			row.DownLimit = &f
		}
		out[s.TsCode] = row
	}
	return out, nil
}

// ===================== 估值截面 / 停牌集合 =====================

// ErrValuationSparse 估值截面空/覆盖不足：本次不得覆盖已有截面。
//
// 触发条件（命中任一即跳过）：
//   - 返回 0 行；
//   - 返回行数 < 在市股票数的一半；
//   - 有效估值行（PE 与 PB 都非 0）< 返回行数的一半。
//
// 为什么不能照写：daily_basic 在 16:30 可能尚未发布（agent_issues #13），
// 旧实现会把全表 PE/PB 覆盖成空值、val_date 盖成当日，估值筛把整池筛光，
// 且 18:00 自检只数 val_date 行数，看不出来。宁保留旧截面并在门禁上显式失败，
// 也不拿一份残缺截面做当日决策。
var ErrValuationSparse = errors.New("估值截面为空或覆盖不足")

// minValuationSample 阈值生效的最小样本量。生产市场约 5,500 只，远高于它；
// 单测/全新库的十几行夹具不启用比例阈值，否则"用 3 只股票造 val_date"
// 这类测试会被当成生产残缺截面。空截面判定不受此下限影响。
const minValuationSample = 100

// checkValuationCoverage 判断一批估值截面是否足以覆盖现有市场。
// listed 为 stock_basic 中 list_status='L' 的行数；为 0 时（尚未同步股票清单）
// 跳过行数覆盖检查，仍检查有效值比例。
func checkValuationCoverage(rows []model.Valuation, listed int) error {
	if len(rows) == 0 {
		return fmt.Errorf("%w: 返回 0 行", ErrValuationSparse)
	}
	if listed >= minValuationSample && len(rows)*2 < listed {
		return fmt.Errorf("%w: 返回 %d 行 < 在市 %d 只的 50%%", ErrValuationSparse, len(rows), listed)
	}
	if len(rows) < minValuationSample {
		return nil
	}
	valid := 0
	for _, v := range rows {
		if v.PETtm != 0 && v.PB != 0 {
			valid++
		}
	}
	if valid*2 < len(rows) {
		return fmt.Errorf("%w: 有效 PE/PB %d/%d 行 < 50%%", ErrValuationSparse, valid, len(rows))
	}
	return nil
}

// SaveValuation 把某交易日的估值截面盖到 stock_basic 的估值列上（一次同步一个整批）。
//
// 只 UPDATE 不 INSERT：不在 stock_basic 里的代码不属于可投 universe（未上市/已摘牌），
// 给它建一行没有读者。整个截面一个事务，避免"一半新一半旧"的截面被选股读到。
// 写入前先过 checkValuationCoverage：残缺截面整批跳过，已有估值保持原样。
func (r *MarketRepo) SaveValuation(ctx context.Context, tradeDate string, rows []model.Valuation) error {
	var listed int
	if err := r.rdb.GetContext(ctx, &listed,
		`SELECT COUNT(*) FROM stock_basic WHERE list_status = 'L'`); err != nil {
		return fmt.Errorf("读取在市股票数失败: %w", err)
	}
	if err := checkValuationCoverage(rows, listed); err != nil {
		return err
	}
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

// CountIndexBarsUpTo 返回指定指数截至 beforeInclusive（含）累计的日线条数。
//
// 与 CountIndexBar 的分工：后者答"今天有没有出数"，本方法答"均线窗口凑不凑得满"。
// 大盘门槛的 MA60 由最近 MarketMAWindow 根现算，凑不满时读取层给 0（不可算），
// 门禁必须能在用上前就看出这一点。
func (r *MarketRepo) CountIndexBarsUpTo(ctx context.Context, tsCode, beforeInclusive string) (int, error) {
	var n int
	q := `SELECT COUNT(*) FROM daily_bar WHERE ts_code = ? AND trade_date <= ?`
	if err := r.rdb.GetContext(ctx, &n, q, tsCode, beforeInclusive); err != nil {
		return 0, fmt.Errorf("统计指数日线累计根数失败: %w", err)
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
