package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

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
			today_bought   INTEGER NOT NULL DEFAULT 0,
			cost_price     REAL NOT NULL DEFAULT 0,
			avg_price      REAL NOT NULL DEFAULT 0,
			updated_at     TEXT NOT NULL
		);`,

		// 持仓元数据（如 initial_capital 等）
		`CREATE TABLE IF NOT EXISTS portfolio_meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);`,

		// 交易计划 (EOD信号/盘中止损产出, Agent读取确认后执行)
		`CREATE TABLE IF NOT EXISTS trade_plan (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			trade_date TEXT NOT NULL,
			ts_code    TEXT NOT NULL,
			name       TEXT,
			direction  TEXT NOT NULL,
			qty        INTEGER NOT NULL,
			ref_price  REAL,
			reason     TEXT,
			strategy   TEXT,
			urgency    TEXT NOT NULL DEFAULT 'normal',
			status     TEXT NOT NULL DEFAULT 'pending',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_trade_plan_date ON trade_plan(trade_date);
		CREATE INDEX IF NOT EXISTS idx_trade_plan_status ON trade_plan(status);`,

		// 智能体辩论结果
		`CREATE TABLE IF NOT EXISTS agent_debate (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			trade_date   TEXT NOT NULL,
			ts_code      TEXT NOT NULL,
			name         TEXT,
			decision     TEXT NOT NULL,
			confidence   REAL,
			position_pct REAL,
			stop_price   REAL,
			risk_level   TEXT,
			bull_args    TEXT,
			bear_args    TEXT,
			risk_note    TEXT,
			summary      TEXT,
			created_at   TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_agent_debate_date ON agent_debate(trade_date);
		CREATE INDEX IF NOT EXISTS idx_agent_debate_code ON agent_debate(ts_code);`,

		// 调度任务执行记录 (防重复执行/启动补跑/健康度展示)
		`CREATE TABLE IF NOT EXISTS job_run (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			job_name    TEXT NOT NULL,
			trade_date  TEXT NOT NULL,
			status      TEXT NOT NULL,
			error       TEXT,
			started_at  TEXT NOT NULL,
			finished_at TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_job_run_name_date ON job_run(job_name, trade_date);`,

		// 智能体通知记录 (告警同时落库, Agent 读取后通知用户)
		`CREATE TABLE IF NOT EXISTS agent_alert (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			trade_date  TEXT NOT NULL,
			job_name    TEXT NOT NULL,
			level       TEXT NOT NULL DEFAULT 'info',
			title       TEXT NOT NULL,
			content     TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'unread',
			created_at  TEXT NOT NULL,
			read_at     TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_agent_alert_date ON agent_alert(trade_date);
		CREATE INDEX IF NOT EXISTS idx_agent_alert_status ON agent_alert(status);`,

		// 自动选股结果
		`CREATE TABLE IF NOT EXISTS screen_result (
			ts_code        TEXT NOT NULL,
			name           TEXT,
			trade_date     TEXT NOT NULL,
			close          REAL,
			pct_chg        REAL,
			turnover_rate  REAL,
			volume_ratio   REAL,
			pe             REAL,
			pe_ttm         REAL,
			pb             REAL,
			circ_mv        REAL,
			score          REAL,
			reason         TEXT,
			PRIMARY KEY (ts_code, trade_date)
		);
		CREATE INDEX IF NOT EXISTS idx_screen_result_date ON screen_result(trade_date);`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("建表失败: %w, sql=%s", err, s)
		}
	}
	return migrateLegacy(db)
}

// migrateLegacy 历史库兼容迁移
// 旧版曾在 account_snapshot(trade_date) 上建全局唯一索引, 多 run 共库时写入冲突;
// 改为 (run_id, trade_date) 唯一
func migrateLegacy(db *sqlx.DB) error {
	var legacy int
	if err := db.Get(&legacy, `SELECT COUNT(1) FROM sqlite_master
		WHERE type='index' AND name='idx_account_snapshot_date'
		AND sql LIKE '%UNIQUE%' AND sql NOT LIKE '%run_id%'`); err == nil && legacy > 0 {
		if _, err := db.Exec(`DROP INDEX idx_account_snapshot_date`); err != nil {
			return fmt.Errorf("删除旧快照唯一索引失败: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_account_snapshot_run_date
		ON account_snapshot(run_id, trade_date)`); err != nil {
		return fmt.Errorf("创建快照唯一索引失败: %w", err)
	}
	// portfolio 表补充 today_bought 列 (T+1 台账, 旧库升级)
	if err := ensureColumn(db, "portfolio", "today_bought", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// portfolio 表补充 high_price 列 (移动止盈历史最高价, 旧库升级)
	if err := ensureColumn(db, "portfolio", "high_price", "REAL NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// trades 表补充 strategy 列 (绩效归因: 每笔成交来源策略, 旧库升级)
	if err := ensureColumn(db, "trades", "strategy", "TEXT"); err != nil {
		return err
	}
	// stock_basic 表补充 industry 列 (行业分散: 真实行业分组, 旧库升级)
	if err := ensureColumn(db, "stock_basic", "industry", "TEXT"); err != nil {
		return err
	}
	return nil
}

// ensureColumn 若列不存在则 ALTER TABLE 添加 (SQLite 不支持 IF NOT EXISTS 加列, 先查 PRAGMA)
func ensureColumn(db *sqlx.DB, table, column, def string) error {
	var count int
	if err := db.Get(&count, `SELECT COUNT(1) FROM pragma_table_info(?) WHERE name = ?`, table, column); err != nil {
		return fmt.Errorf("查询表结构失败(%s.%s): %w", table, column, err)
	}
	if count > 0 {
		return nil
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, column, def)); err != nil {
		return fmt.Errorf("添加列失败(%s.%s): %w", table, column, err)
	}
	return nil
}
