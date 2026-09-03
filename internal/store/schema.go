package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// schemaDDL 全部建表 DDL（幂等 IF NOT EXISTS），严格对应 ARCHITECTURE §3 定义。
// 共 28 张表：配置(1) + 市场(9) + 财务(3) + 选股信号(4) + 交易(4) + 目标(2)
// + 运维(4) + LLM(1)。字段与 §3 逐字段一致，禁止自行增删。
var schemaDDL = []string{
	// ===================== 3.1 配置域 =====================
	`CREATE TABLE IF NOT EXISTS config_kv (
		key         TEXT PRIMARY KEY,
		value       TEXT NOT NULL,
		value_type  TEXT NOT NULL DEFAULT 'string',
		is_secret   INTEGER NOT NULL DEFAULT 0,
		updated_at  TEXT NOT NULL,
		updated_by  TEXT NOT NULL DEFAULT 'system'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_config_secret ON config_kv(is_secret)`,

	// ===================== 3.2 市场域 =====================
	`CREATE TABLE IF NOT EXISTS trade_cal (
		cal_date        TEXT PRIMARY KEY,
		is_open         INTEGER NOT NULL,
		pretrade_date   TEXT,
		nexttrade_date  TEXT,
		exchange        TEXT NOT NULL DEFAULT 'SSE'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_cal_open_date ON trade_cal(is_open, cal_date)`,

	`CREATE TABLE IF NOT EXISTS stock_basic (
		ts_code      TEXT PRIMARY KEY,
		symbol       TEXT NOT NULL,
		name         TEXT NOT NULL,
		market       TEXT,
		exchange     TEXT,
		industry     TEXT,
		list_date    TEXT,
		delist_date  TEXT,
		is_st        INTEGER NOT NULL DEFAULT 0,
		list_status  TEXT,
		updated_at   TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_stock_industry ON stock_basic(industry)`,

	`CREATE TABLE IF NOT EXISTS daily_bar (
		ts_code     TEXT NOT NULL,
		trade_date  TEXT NOT NULL,
		open        INTEGER NOT NULL,
		high        INTEGER NOT NULL,
		low         INTEGER NOT NULL,
		close       INTEGER NOT NULL,
		pre_close   INTEGER NOT NULL,
		pct_chg     REAL,
		vol_lot     REAL,
		amount_k    REAL,
		adj_factor  REAL,
		raw_close   INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (ts_code, trade_date)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_bar_date ON daily_bar(trade_date)`,
	`CREATE INDEX IF NOT EXISTS idx_bar_code ON daily_bar(ts_code)`,

	`CREATE TABLE IF NOT EXISTS daily_basic (
		ts_code        TEXT NOT NULL,
		trade_date     TEXT NOT NULL,
		close          INTEGER,
		turnover_rate  REAL,
		volume_ratio   REAL,
		pe             REAL,
		pe_ttm         REAL,
		pb             REAL,
		ps_ttm         REAL,
		dv_ratio       REAL,
		total_mv_w     REAL,
		circ_mv_w      REAL,
		PRIMARY KEY (ts_code, trade_date)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_basic_date ON daily_basic(trade_date)`,

	`CREATE TABLE IF NOT EXISTS stk_limit (
		ts_code     TEXT NOT NULL,
		trade_date  TEXT NOT NULL,
		up_limit    INTEGER NOT NULL,
		down_limit  INTEGER NOT NULL,
		PRIMARY KEY (ts_code, trade_date)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_limit_date ON stk_limit(trade_date)`,

	`CREATE TABLE IF NOT EXISTS suspend_d (
		ts_code        TEXT NOT NULL,
		trade_date     TEXT NOT NULL,
		suspend_type   TEXT,
		suspend_timing TEXT,
		PRIMARY KEY (ts_code, trade_date)
	)`,

	`CREATE TABLE IF NOT EXISTS adj_factor (
		ts_code    TEXT NOT NULL,
		trade_date TEXT NOT NULL,
		adj_factor REAL NOT NULL,
		PRIMARY KEY (ts_code, trade_date)
	)`,

	`CREATE TABLE IF NOT EXISTS index_daily (
		ts_code    TEXT NOT NULL,
		trade_date TEXT NOT NULL,
		close      INTEGER NOT NULL,
		ma20       REAL,
		PRIMARY KEY (ts_code, trade_date)
	)`,

	`CREATE TABLE IF NOT EXISTS moneyflow (
		ts_code         TEXT NOT NULL,
		trade_date      TEXT NOT NULL,
		buy_elg_amount  REAL,
		sell_elg_amount REAL,
		net_mf_amount   REAL,
		PRIMARY KEY (ts_code, trade_date)
	)`,

	// ===================== 3.3 财务域（慢路径）=====================
	`CREATE TABLE IF NOT EXISTS fina_indicator (
		ts_code            TEXT NOT NULL,
		end_date           TEXT NOT NULL,
		ann_date           TEXT NOT NULL,
		eps                REAL,
		roe                REAL,
		roe_dt             REAL,
		grossprofit_margin REAL,
		netprofit_margin   REAL,
		debt_to_assets     REAL,
		netprofit_yoy      REAL,
		or_yoy             REAL,
		bps                REAL,
		PRIMARY KEY (ts_code, end_date)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fina_ann ON fina_indicator(ann_date)`,
	`CREATE INDEX IF NOT EXISTS idx_fina_code ON fina_indicator(ts_code)`,

	`CREATE TABLE IF NOT EXISTS fina_sync_state (
		id              INTEGER PRIMARY KEY CHECK (id = 1),
		status          TEXT NOT NULL,
		cursor_ts_code  TEXT,
		total           INTEGER NOT NULL DEFAULT 0,
		done            INTEGER NOT NULL DEFAULT 0,
		failed          INTEGER NOT NULL DEFAULT 0,
		started_at      TEXT,
		finished_at     TEXT
	)`,

	`CREATE TABLE IF NOT EXISTS fina_sync_item (
		ts_code           TEXT PRIMARY KEY,
		last_sync_end_date TEXT,
		status            TEXT NOT NULL,
		attempts          INTEGER NOT NULL DEFAULT 0,
		last_error        TEXT,
		updated_at        TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fina_item_status ON fina_sync_item(status)`,

	// ===================== 3.4 选股与信号域 =====================
	`CREATE TABLE IF NOT EXISTS screen_result (
		trade_date     TEXT NOT NULL,
		ts_code        TEXT NOT NULL,
		rank           INTEGER NOT NULL,
		score          REAL NOT NULL,
		f_momentum     REAL NOT NULL DEFAULT 0,
		f_quality      REAL NOT NULL DEFAULT 0,
		f_value        REAL NOT NULL DEFAULT 0,
		f_lowvol       REAL NOT NULL DEFAULT 0,
		f_liquidity    REAL NOT NULL DEFAULT 0,
		close          INTEGER NOT NULL,
		circ_mv_w      REAL,
		pe_ttm         REAL,
		pb             REAL,
		turnover_rate  REAL,
		reason         TEXT,
		PRIMARY KEY (trade_date, ts_code)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_screen_date_rank ON screen_result(trade_date, rank)`,

	`CREATE TABLE IF NOT EXISTS signal (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date  TEXT NOT NULL,
		ts_code     TEXT NOT NULL,
		name        TEXT NOT NULL,
		direction   TEXT NOT NULL,
		rule        TEXT NOT NULL,
		confidence  REAL,
		ref_price   INTEGER NOT NULL,
		reason      TEXT NOT NULL,
		payload     TEXT,
		status      TEXT NOT NULL DEFAULT 'new',
		reject_rule TEXT,
		reject_msg  TEXT,
		created_at  TEXT NOT NULL,
		UNIQUE (trade_date, ts_code, direction, rule)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_signal_date ON signal(trade_date, status)`,

	`CREATE TABLE IF NOT EXISTS screen_funnel (
		trade_date      TEXT NOT NULL,
		stage           INTEGER NOT NULL,
		stage_name      TEXT NOT NULL,
		passed_in       INTEGER NOT NULL,
		passed_out      INTEGER NOT NULL,
		dropped         INTEGER NOT NULL,
		drop_reasons    TEXT,
		thresholds      TEXT,
		PRIMARY KEY (trade_date, stage)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_funnel_date ON screen_funnel(trade_date, stage)`,

	`CREATE TABLE IF NOT EXISTS screen_watchlist (
		trade_date      TEXT NOT NULL,
		ts_code         TEXT NOT NULL,
		rank            INTEGER NOT NULL,
		score           REAL NOT NULL,
		reason          TEXT,
		PRIMARY KEY (trade_date, ts_code)
	)`,

	// ===================== 3.5 交易域 =====================
	`CREATE TABLE IF NOT EXISTS order_ticket (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date     TEXT NOT NULL,
		ts_code        TEXT NOT NULL,
		name           TEXT NOT NULL,
		direction      TEXT NOT NULL,
		qty            INTEGER NOT NULL,
		ref_price_low  INTEGER NOT NULL,
		ref_price_high INTEGER NOT NULL,
		stop_price     INTEGER,
		reason         TEXT NOT NULL,
		position_pct   REAL,
		urgency        TEXT NOT NULL DEFAULT 'normal',
		source         TEXT NOT NULL,
		status         TEXT NOT NULL DEFAULT 'drafted',
		valid_until    TEXT NOT NULL,
		gear           TEXT NOT NULL,
		profit_lock    INTEGER NOT NULL DEFAULT 0,
		goal_snapshot  TEXT,
		signal_id      INTEGER,
		skip_reason    TEXT,
		created_at     TEXT NOT NULL,
		updated_at     TEXT NOT NULL,
		issued_at      TEXT,
		closed_at      TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ticket_date_status ON order_ticket(trade_date, status)`,
	`CREATE INDEX IF NOT EXISTS idx_ticket_status_until ON order_ticket(status, valid_until)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_ticket_active
		ON order_ticket(trade_date, ts_code, direction)
		WHERE status IN ('drafted','issued')`,

	`CREATE TABLE IF NOT EXISTS fill (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		ticket_id    INTEGER NOT NULL UNIQUE,
		ts_code      TEXT NOT NULL,
		direction    TEXT NOT NULL,
		qty          INTEGER NOT NULL,
		price        INTEGER NOT NULL,
		amount       INTEGER NOT NULL,
		commission   INTEGER NOT NULL,
		stamp_tax    INTEGER NOT NULL,
		transfer_fee INTEGER NOT NULL,
		total_cost   INTEGER NOT NULL,
		trade_date   TEXT NOT NULL,
		reported_by  TEXT NOT NULL,
		reported_at  TEXT NOT NULL,
		note         TEXT,
		FOREIGN KEY (ticket_id) REFERENCES order_ticket(id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fill_date ON fill(trade_date)`,

	`CREATE TABLE IF NOT EXISTS position (
		ts_code       TEXT PRIMARY KEY,
		total_qty     INTEGER NOT NULL DEFAULT 0,
		available_qty INTEGER NOT NULL DEFAULT 0,
		today_bought  INTEGER NOT NULL DEFAULT 0,
		cost_price    INTEGER NOT NULL DEFAULT 0,
		high_price    INTEGER NOT NULL DEFAULT 0,
		first_open_date TEXT,
		updated_at    TEXT NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS account_snapshot (
		trade_date     TEXT PRIMARY KEY,
		cash           INTEGER NOT NULL,
		market_value   INTEGER NOT NULL,
		total_asset    INTEGER NOT NULL,
		position_count INTEGER NOT NULL,
		gear           TEXT NOT NULL,
		profit_lock    INTEGER NOT NULL DEFAULT 0,
		created_at     TEXT NOT NULL
	)`,

	// ===================== 3.6 目标域 =====================
	`CREATE TABLE IF NOT EXISTS goal_state (
		id                INTEGER PRIMARY KEY CHECK (id = 1),
		quarter           TEXT NOT NULL,
		quarter_start     TEXT NOT NULL,
		quarter_end       TEXT NOT NULL,
		baseline_asset    INTEGER NOT NULL DEFAULT 0,
		peak_asset        INTEGER NOT NULL DEFAULT 0,
		current_gear      TEXT NOT NULL DEFAULT 'G1',
		profit_lock       INTEGER NOT NULL DEFAULT 0,
		upgrade_streak    INTEGER NOT NULL DEFAULT 0,
		last_eval_date    TEXT,
		override_gear     TEXT,
		override_reason   TEXT,
		override_until    TEXT,
		pace_policy       TEXT NOT NULL DEFAULT 'unrestricted',
		pace_confirm_date TEXT,
		updated_at        TEXT NOT NULL
	)`,

	`CREATE TABLE IF NOT EXISTS goal_gear_log (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date      TEXT NOT NULL,
		quarter         TEXT NOT NULL,
		from_gear       TEXT NOT NULL,
		to_gear         TEXT NOT NULL,
		from_lock       INTEGER NOT NULL DEFAULT 0,
		to_lock         INTEGER NOT NULL DEFAULT 0,
		trigger_rule    TEXT NOT NULL,
		progress        REAL,
		budget_consumed REAL,
		pace_gap        REAL,
		is_manual       INTEGER NOT NULL DEFAULT 0,
		reason          TEXT,
		params_snapshot TEXT,
		created_at      TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_gearlog_date ON goal_gear_log(trade_date, created_at)`,

	// ===================== 3.7 运维域 =====================
	`CREATE TABLE IF NOT EXISTS job_run (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		job_name    TEXT NOT NULL,
		trade_date  TEXT NOT NULL,
		attempt     INTEGER NOT NULL DEFAULT 1,
		status      TEXT NOT NULL,
		duration_ms INTEGER,
		error       TEXT,
		artifacts   TEXT,
		degradations TEXT,
		started_at  TEXT NOT NULL,
		finished_at TEXT,
		UNIQUE (job_name, trade_date, attempt)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_jobrun_name_date ON job_run(job_name, trade_date, started_at)`,

	`CREATE TABLE IF NOT EXISTS agent_alert (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date TEXT NOT NULL,
		source     TEXT NOT NULL,
		level      TEXT NOT NULL,
		code       TEXT NOT NULL,
		title      TEXT NOT NULL,
		content    TEXT NOT NULL,
		status     TEXT NOT NULL DEFAULT 'unread',
		created_at TEXT NOT NULL,
		read_at    TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_alert_date_level ON agent_alert(trade_date, level)`,
	`CREATE INDEX IF NOT EXISTS idx_alert_status ON agent_alert(status)`,
	`CREATE INDEX IF NOT EXISTS idx_alert_dedup ON agent_alert(code, created_at)`,

	`CREATE TABLE IF NOT EXISTS action_log (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date   TEXT NOT NULL,
		actor        TEXT NOT NULL,
		object_type  TEXT NOT NULL,
		object_id    TEXT,
		action       TEXT NOT NULL,
		before_value TEXT,
		after_value  TEXT,
		reason       TEXT,
		created_at   TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_actionlog_date ON action_log(trade_date)`,
	`CREATE INDEX IF NOT EXISTS idx_actionlog_object ON action_log(object_type, object_id)`,

	`CREATE TABLE IF NOT EXISTS mail_outbox (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date   TEXT NOT NULL,
		mail_type    TEXT NOT NULL,
		subject      TEXT NOT NULL,
		body         TEXT NOT NULL,
		status       TEXT NOT NULL DEFAULT 'pending',
		attempts     INTEGER NOT NULL DEFAULT 0,
		last_error   TEXT,
		next_retry_at TEXT,
		created_at   TEXT NOT NULL,
		sent_at      TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_mail_status_retry ON mail_outbox(status, next_retry_at)`,
	`CREATE INDEX IF NOT EXISTS idx_mail_date_type ON mail_outbox(trade_date, mail_type)`,

	// ===================== 3.8 LLM 域（P1，表结构预留）=====================
	`CREATE TABLE IF NOT EXISTS llm_call (
		id             INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date     TEXT NOT NULL,
		signal_id      INTEGER,
		ts_code        TEXT NOT NULL,
		verdict        TEXT,
		confidence     REAL,
		rationale      TEXT,
		status         TEXT NOT NULL,
		error          TEXT,
		review_date    TEXT,
		review_ret_pct REAL,
		review_correct INTEGER,
		created_at     TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_llmcall_date ON llm_call(trade_date)`,
}

// CreateTables 执行全部建表 DDL（幂等）。
func CreateTables(db *sqlx.DB) error {
	for _, ddl := range schemaDDL {
		if _, err := db.Exec(ddl); err != nil {
			return fmt.Errorf("建表失败: %w\nDDL: %s", err, ddl)
		}
	}
	return nil
}

// TableCount 返回当前库中用户表数量（验收 #6）。
func TableCount(db *sqlx.DB) (int, error) {
	var n int
	err := db.Get(&n, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'schema_version'")
	if err != nil {
		return 0, fmt.Errorf("统计表数量失败: %w", err)
	}
	return n, nil
}
