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
			delist_date  TEXT,
			industry     TEXT
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
			strategy     TEXT,
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
		CREATE INDEX IF NOT EXISTS idx_account_snapshot_run_id ON account_snapshot(run_id);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_account_snapshot_run_date ON account_snapshot(run_id, trade_date);`,

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
		);`,
		// 按 trade_date 的索引服务于全市场单日查询, 该查询路径已不存在;
		// 留在库里只是每次同步多写一份 67MB, 历史库启动时自行回收
		`DROP INDEX IF EXISTS idx_moneyflow_date;`,

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

		// 持仓（持久化，API 重启后不丢失）; 旧库残留的 avg_price 列已废弃, 应用层不再读写
		`CREATE TABLE IF NOT EXISTS portfolio (
			ts_code        TEXT PRIMARY KEY,
			total_qty      INTEGER NOT NULL DEFAULT 0,
			available_qty  INTEGER NOT NULL DEFAULT 0,
			today_bought   INTEGER NOT NULL DEFAULT 0,
			high_price     REAL NOT NULL DEFAULT 0,
			cost_price     REAL NOT NULL DEFAULT 0,
			updated_at     TEXT NOT NULL
		);`,

		// 配置键值表 (运行期配置: initial_capital/cash/goal_risk_mode 等; 原 portfolio_meta 迁移而来)
		`CREATE TABLE IF NOT EXISTS config_kv (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT (datetime('now'))
		);`,

		// 统一动作日志 (任务/接口/人工成交执行记录; 每动作一条, 可按日汇总)
		`CREATE TABLE IF NOT EXISTS action_log (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			trade_date  TEXT NOT NULL,
			kind        TEXT NOT NULL,
			name        TEXT NOT NULL,
			status      TEXT NOT NULL,
			summary     TEXT,
			detail      TEXT,
			duration_ms INTEGER,
			created_at  TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_action_log_date ON action_log(trade_date);
		CREATE INDEX IF NOT EXISTS idx_action_log_name ON action_log(name);`,

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

		// 辩论决策复盘 (T+5 收益回填, 反思闭环数据源; debate_id 唯一防重复回填)
		`CREATE TABLE IF NOT EXISTS agent_debate_review (
					id          INTEGER PRIMARY KEY AUTOINCREMENT,
					debate_id   INTEGER NOT NULL,
					trade_date  TEXT NOT NULL,
					ts_code     TEXT NOT NULL,
					decision    TEXT NOT NULL,
					confidence  REAL NOT NULL DEFAULT 0,
					base_close  REAL NOT NULL DEFAULT 0,
					review_date TEXT NOT NULL,
					last_close  REAL NOT NULL DEFAULT 0,
					ret_pct     REAL NOT NULL DEFAULT 0,
					correct     INTEGER NOT NULL DEFAULT 0
				);
				CREATE INDEX IF NOT EXISTS idx_debate_review_code ON agent_debate_review(ts_code);
				CREATE UNIQUE INDEX IF NOT EXISTS idx_debate_review_debate ON agent_debate_review(debate_id);`,

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

		// 新闻去重: major_news 在低积分档位只提供"最新一页"(实测约 800 条, 覆盖最近约 1.5 天),
		// 相邻两天的拉取必然大量重叠。news 表原先没有唯一约束, INSERT OR IGNORE 实际上永不忽略,
		// 重叠部分会逐日重复堆积。先清掉历史重复, 再建唯一索引, 把去重交给约束本身。
		`DELETE FROM news WHERE id NOT IN (
			SELECT MIN(id) FROM news GROUP BY datetime, title
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_news_dedup ON news(datetime, title);`,
	}

	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("建表失败: %w, sql=%s", err, s)
		}
	}
	return nil
}
