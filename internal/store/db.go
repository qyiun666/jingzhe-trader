package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动, 无需 CGO
)

// NewDB 打开 SQLite 数据库并执行建表迁移
// path 为数据库文件路径, 不存在时会自动创建
func NewDB(path string) (*sqlx.DB, error) {
	db, err := sqlx.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 连接存活检查
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("数据库连接失败: %w", err)
	}

	// SQLite 性能与并发优化
	pragmas := []string{
		"PRAGMA journal_mode=WAL;",   // 写前日志, 提升并发读写
		"PRAGMA synchronous=NORMAL;", // WAL 模式下安全且更快
		"PRAGMA busy_timeout=5000;",  // 锁等待 5 秒
		"PRAGMA foreign_keys=ON;",    // 开启外键约束
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %s 失败: %w", p, err)
		}
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// migrate 执行建表 SQL(幂等, 使用 IF NOT EXISTS)
func migrate(db *sqlx.DB) error {
	stmts := []string{
		// 股票基本信息
		`CREATE TABLE IF NOT EXISTS stock_basic (
			ts_code      TEXT PRIMARY KEY,
			symbol       TEXT,
			name         TEXT,
			market       TEXT,
			exchange     TEXT,
			is_st        INTEGER,
			list_status  TEXT,
			list_date    TEXT,
			delist_date  TEXT
		);`,

		// 交易日历
		`CREATE TABLE IF NOT EXISTS trade_cal (
			cal_date       TEXT PRIMARY KEY,
			is_open        INTEGER,
			pretrade_date  TEXT,
			exchange       TEXT
		);`,

		// 日线行情
		`CREATE TABLE IF NOT EXISTS daily_bar (
			ts_code     TEXT NOT NULL,
			trade_date  TEXT NOT NULL,
			open        REAL,
			high        REAL,
			low         REAL,
			close       REAL,
			pre_close   REAL,
			change      REAL,
			pct_chg     REAL,
			vol         REAL,
			amount      REAL,
			adj_factor  REAL,
			PRIMARY KEY (ts_code, trade_date)
		);
		CREATE INDEX IF NOT EXISTS idx_daily_bar_trade_date ON daily_bar(trade_date);
		CREATE INDEX IF NOT EXISTS idx_daily_bar_ts_code ON daily_bar(ts_code);`,

		// 每日基本面
		`CREATE TABLE IF NOT EXISTS daily_basic (
			ts_code        TEXT NOT NULL,
			trade_date     TEXT NOT NULL,
			close          REAL,
			turnover_rate  REAL,
			volume_ratio   REAL,
			pe             REAL,
			pe_ttm         REAL,
			pb             REAL,
			ps             REAL,
			ps_ttm         REAL,
			dv_ratio       REAL,
			total_mv       REAL,
			circ_mv        REAL,
			limit_status   INTEGER,
			PRIMARY KEY (ts_code, trade_date)
		);
		CREATE INDEX IF NOT EXISTS idx_daily_basic_trade_date ON daily_basic(trade_date);`,

		// 财务指标
		`CREATE TABLE IF NOT EXISTS fina_indicator (
			ts_code             TEXT NOT NULL,
			end_date            TEXT NOT NULL,
			ann_date            TEXT,
			eps                 REAL,
			roe                 REAL,
			grossprofit_margin  REAL,
			netprofit_margin    REAL,
			debt_to_assets      REAL,
			netprofit_yoy       REAL,
			or_yoy              REAL,
			bps                 REAL,
			PRIMARY KEY (ts_code, end_date)
		);
		CREATE INDEX IF NOT EXISTS idx_fina_code ON fina_indicator(ts_code);`,

		// 涨跌停价
		`CREATE TABLE IF NOT EXISTS stk_limit (
			ts_code     TEXT NOT NULL,
			trade_date  TEXT NOT NULL,
			up_limit    REAL,
			down_limit  REAL,
			PRIMARY KEY (ts_code, trade_date)
		);`,

		// 订单
		`CREATE TABLE IF NOT EXISTS orders (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id       TEXT,
			ts_code      TEXT,
			side         INTEGER,
			price        REAL,
			qty          INTEGER,
			filled_qty   INTEGER,
			avg_price    REAL,
			status       INTEGER,
			reason       TEXT,
			create_time  TEXT,
			update_time  TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_orders_run_id ON orders(run_id);`,

		// 成交记录
		`CREATE TABLE IF NOT EXISTS trades (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id       TEXT,
			order_id     INTEGER,
			ts_code      TEXT,
			side         INTEGER,
			price        REAL,
			qty          INTEGER,
			amount       REAL,
			commission   REAL,
			stamp_tax    REAL,
			transfer_fee REAL,
			total_cost   REAL,
			trade_date   TEXT,
			trade_time   TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_trades_run_id ON trades(run_id);`,

		// 账户快照
		`CREATE TABLE IF NOT EXISTS account_snapshot (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id         TEXT,
			trade_date     TEXT,
			total_asset    REAL,
			cash           REAL,
			market_value   REAL,
			pnl            REAL,
			pnl_pct        REAL,
			total_pnl      REAL,
			total_pnl_pct  REAL
		);
		CREATE INDEX IF NOT EXISTS idx_account_snapshot_run_id ON account_snapshot(run_id);`,

		// 持仓快照
		`CREATE TABLE IF NOT EXISTS position_snapshot (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id         TEXT,
			trade_date     TEXT,
			ts_code        TEXT,
			total_qty      INTEGER,
			available_qty  INTEGER,
			cost_price     REAL,
			market_price   REAL,
			market_value   REAL,
			floating_pnl   REAL
		);
		CREATE INDEX IF NOT EXISTS idx_position_snapshot_run_id ON position_snapshot(run_id);`,

		// 新股申购
		`CREATE TABLE IF NOT EXISTS new_shares (
			ts_code TEXT PRIMARY KEY,
			sub_code TEXT,
			name TEXT,
			ipo_date TEXT,
			issue_date TEXT,
			amount REAL,
			market_amount REAL,
			price REAL,
			pe REAL,
			limit_amount REAL,
			funds REAL,
			ballot REAL
		);`,

		// 新闻快讯
		`CREATE TABLE IF NOT EXISTS news (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			datetime TEXT,
			content TEXT,
			title TEXT,
			channels TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_news_datetime ON news(datetime);`,

		// 资金流向
		`CREATE TABLE IF NOT EXISTS moneyflow (
			ts_code TEXT,
			trade_date TEXT,
			buy_elg_amount REAL,
			sell_elg_amount REAL,
			net_mf_amount REAL,
			PRIMARY KEY (ts_code, trade_date)
		);
		CREATE INDEX IF NOT EXISTS idx_moneyflow_date ON moneyflow(trade_date);`,

		// 龙虎榜
		`CREATE TABLE IF NOT EXISTS top_list (
			ts_code TEXT,
			trade_date TEXT,
			name TEXT,
			close REAL,
			pct_change REAL,
			turnover_rate REAL,
			amount REAL,
			net_amount REAL,
			buy_amount REAL,
			sell_amount REAL,
			PRIMARY KEY (ts_code, trade_date)
		);
		CREATE INDEX IF NOT EXISTS idx_toplist_date ON top_list(trade_date);`,

		// 持仓（持久化，API 重启后不丢失）
		`CREATE TABLE IF NOT EXISTS portfolio (
			ts_code        TEXT PRIMARY KEY,
			total_qty      INTEGER NOT NULL DEFAULT 0,
			available_qty  INTEGER NOT NULL DEFAULT 0,
			cost_price     REAL NOT NULL DEFAULT 0,
			avg_price      REAL NOT NULL DEFAULT 0,
			updated_at     TEXT NOT NULL
		);`,

		// 持仓元数据（如 initial_capital 等）
		`CREATE TABLE IF NOT EXISTS portfolio_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("建表失败: %w, sql=%s", err, s)
		}
	}
	return nil
}
