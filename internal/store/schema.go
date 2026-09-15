package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// schemaDDL 全部建表 DDL（幂等 IF NOT EXISTS）。共 7 张表，本文件即结构的唯一真相源
// （迁移体系已删除：库不跨版本升级，就没有第二份结构需要调和）。
//
//	状态 1  config_kv    key→value：配置键 + 单行状态 + 当日集合
//	市场 3  trade_cal    stock_basic    daily_bar
//	交易 2  order_ticket position
//	轨迹 1  run_trace    任务 / 邮件 / 告警 / LLM 调用，一天一件事一行
//
// 建表的判据有两条，缺一律不建：
//  1. 有点名的读者（哪个函数按什么条件读它）；
//  2. 粒度或保留窗口与已有表不同 —— 字段长得像不算理由，能从别的表或本表别的列算出来的
//     数就不落库（"一个数据只存一处"）。
//
// 按这两条折叠掉的三张表（不是删信息，是换存放位置）：
//   - llm_call → run_trace：唯一键 (trade_date, ts_code, prompt_key) 就是 run_trace 的
//     UNIQUE(trade_date, subject)，status 就是 outcome，窗口同为 90 天。见 repo_llm.go：
//     subject = llm:<标的>:<prompt_key>，结论与理由序列化进 detail。
//   - suspend_d → config_kv：两个读者都只要"当日停牌代码集合"，一天一行
//     suspend:<YYYYMMDD> 存逗号分隔代码。见 MarketRepo.SaveSuspended。
//   - goal_state → config_kv：只有一行、且每次写都整行覆盖（没有按列更新、没有按列查询）。
//     见 repo_goal.go 的 goal.state 键。
//   - daily_basic → stock_basic：估值截面每票每日一份，但全项目只有"选股当日"这一个读者，
//     没有任何历史复算路径读旧日期；它与 stock_basic 同为"按 ts_code 的当前快照"，合并后
//     省一张表、两个索引、一条保留规则。代价：补跑旧一日选股时估值是更新的，故 screener
//     显式核对 val_date，不匹配就报数据不符而不是拿新估值凑。
//
// 明确不入库的数据：
//   - 选股过程产物（候选、信号、漏斗计数）：选股是一条顺序流水线，中间结果在内存里交给
//     决策环节，每级进出数写日志。落库的结果只有指令单。
//   - 过程状态（任务尝试次数、耗时、产出物明细、降级清单、告警已读标记）：并入 run_trace
//     的 outcome + detail 两列，一次写覆盖一行。
//   - 邮件发件队列：发信同步一次，结果写一行 run_trace。不再跨进程补发 —— 那是为
//     "SMTP 半夜挂了"设计的机制，而 SMTP 挂掉会在当日轨迹里显式失败。
//   - 成交表：一单最多一回执（原 fill.ticket_id 的 UNIQUE 约束即为此），成交字段直接落在
//     order_ticket 行上，避免 ts_code/direction/trade_date 三列从单据抄写。
//   - 派生值一律不落列（存了就会出现两处不一致）：可用数量 = total_qty − today_bought；
//     成交金额 = fill_qty × fill_price；三项费用 = 金额 × 费率配置；前后交易日 = 日历行序；
//     是否 ST = 股票名称前缀；现金余额 = 本金/锚点 − Σ含费成交合计。
//   - 日线开高低走、涨跌幅、成交额、复权因子：全链路零消费者。量比由 vol_lot 现算，
//     均线由 index 收盘现算。
//   - 指数独立表：与 daily_bar 同形同窗口、代码不重叠，直接共用 daily_bar。
//   - 停牌并入 daily_bar：实测 190 行停牌记录里 145 行当日没有对应日线（停牌股不出线），
//     而这 145 行正是覆盖缺口闸门豁免"该有却没行情"的依据，并列会把它丢掉。
//   - 涨跌停价、个股资金流、财务快照、账户日终快照、档位变更历史、资产净值曲线、新闻公告：
//     见 ARCHITECTURE §3 各条删表理由。
var schemaDDL = []string{
	// ===================== 配置与状态 =====================
	// 只有两列：类型/凭据标记/改动人与时间都不存 —— 类型的唯一真相源是代码里的键目录
	// （config.KeySpecs），库里那份镜像零读者；谁改了配置走服务日志。
	// 除配置键外还承载三类机器写入的状态：goal.state、suspend:<YYYYMMDD>、
	// account.cash_anchor*。它们不进键目录，config set 拒绝写、config dump 不显示。
	`CREATE TABLE IF NOT EXISTS config_kv (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,

	// ===================== 市场 =====================
	// 只存"这天开不开市"。前后交易日按 cal_date 行序现算（market.PrevTradeDay/NextTradeDay），
	// 原 pretrade_date/nexttrade_date 是从 Tushare 抄回来却没人读的镜像。
	// synthetic 唯一的读者是"真实日历续上以后整批清掉补齐行重建"。
	`CREATE TABLE IF NOT EXISTS trade_cal (
		cal_date  TEXT PRIMARY KEY,
		is_open   INTEGER NOT NULL,
		synthetic INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_cal_open_date ON trade_cal(is_open, cal_date)`,

	// 每只票一行的当前快照：静态属性 + 最近一个交易日的估值截面（val_date 标注是哪天）。
	// 指数不在本表（指数没有估值口径），只在 daily_bar 里有线。
	// 是否 ST 不落列：ST 判定看名称前缀（market.IsSTName），名称就是唯一真相源。
	`CREATE TABLE IF NOT EXISTS stock_basic (
		ts_code       TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		industry      TEXT,
		list_date     TEXT,
		list_status   TEXT,
		val_date      TEXT,
		turnover_rate REAL,
		pe_ttm        REAL,
		pb            REAL,
		circ_mv_w     REAL
	)`,

	// 个股与指数共用：指数代码（000001.SH 等）不与个股重叠，指数的 vol_lot/raw_close 为 0。
	// 主键 (ts_code, trade_date) 已覆盖所有以 ts_code 打头的查询，不再另建 ts_code 索引。
	//
	// WITHOUT ROWID：本表是全库唯一的窄列复合主键表，主键即唯一访问路径
	// （按 ts_code+日期取序列、按 trade_date 取截面）。普通表要为这张表额外维护一棵
	// 10MB 量级的主键索引（2026-09-15 实测：177K 行时主键索引 10.0MB，表数据 6.8MB），
	// 而 WITHOUT ROWID 让主键 B 树本身就是表，省掉这棵索引。
	// 代价：分批删除不能再走 rowid 子查询（见 tx.go 的 DeleteBatchedRows）。
	`CREATE TABLE IF NOT EXISTS daily_bar (
		ts_code     TEXT NOT NULL,
		trade_date  TEXT NOT NULL,
		close       INTEGER NOT NULL,
		vol_lot     REAL,
		raw_close   INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (ts_code, trade_date)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS idx_bar_date ON daily_bar(trade_date)`,

	// ===================== 交易 =====================
	// 成交回执直接落在本行（一单最多一回执）。不落 stop_price / 仓位比例 / 来源 / 时间戳：
	// 止损与占比由风控参数按当时档位随时可重算，"哪条规则提的"进日志，流转时刻进日志。
	// 费用明细同理不落列：只有含费合计 total_cost 是"这笔实际占用/到账多少现金"的结果。
	// note 是唯一的人工备注列：作废原因与成交备注都写它（两个写者，查单时读）。
	`CREATE TABLE IF NOT EXISTS order_ticket (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date   TEXT NOT NULL,
		ts_code      TEXT NOT NULL,
		name         TEXT NOT NULL,
		direction    TEXT NOT NULL,
		qty          INTEGER NOT NULL,
		ref_price    INTEGER NOT NULL,
		reason       TEXT NOT NULL,
		status       TEXT NOT NULL DEFAULT 'drafted',
		valid_until  TEXT NOT NULL,
		gear         TEXT NOT NULL,
		fill_qty     INTEGER NOT NULL DEFAULT 0,
		fill_price   INTEGER NOT NULL DEFAULT 0,
		total_cost   INTEGER NOT NULL DEFAULT 0,
		reported_by  TEXT,
		reported_at  TEXT,
		note         TEXT
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ticket_date_status ON order_ticket(trade_date, status)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_ticket_active
		ON order_ticket(trade_date, ts_code, direction)
		WHERE status IN ('drafted','issued')`,

	// 可用数量不落列：恒等于 total_qty − today_bought（T+1），见 Position.Available()。
	// high_price 不落日线是因为持仓可以比日线保留窗口（45 天）更久，超出后无法回算。
	`CREATE TABLE IF NOT EXISTS position (
		ts_code         TEXT PRIMARY KEY,
		total_qty       INTEGER NOT NULL DEFAULT 0,
		today_bought    INTEGER NOT NULL DEFAULT 0,
		cost_price      INTEGER NOT NULL DEFAULT 0,
		high_price      INTEGER NOT NULL DEFAULT 0,
		first_open_date TEXT
	)`,

	// ===================== 轨迹 =====================
	// 一件事一天一行、重跑覆盖：留下的就是"今天这件做成没有"。
	// subject 命名空间：job:<任务> / mail:<类型> / alert:<码> / llm:<标的>:<prompt_key>。
	`CREATE TABLE IF NOT EXISTS run_trace (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		trade_date TEXT NOT NULL,
		subject    TEXT NOT NULL,
		outcome    TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT '',
		at         TEXT NOT NULL,
		UNIQUE (trade_date, subject)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_trace_subject ON run_trace(subject, at)`,
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

// SchemaTables 现役表清单（schemaDDL 的权威镜像，供启动期遗留对象清点与单测断言使用）。
//
// 它存在的原因是 CREATE TABLE IF NOT EXISTS 只做加法：跨版本重建后，老二进制留下的
// 表与索引会永远留在库里，既占空间又让人以为还有读者。启动期拿这份清单跟
// sqlite_master 对一遍，多出来的就是遗留物（见 audit.go）。
var SchemaTables = []string{
	"config_kv", "daily_bar", "order_ticket", "position", "run_trace", "stock_basic", "trade_cal",
}

// SchemaIndexes 现役具名索引清单（不含 SQLite 自动创建的主键索引 sqlite_autoindex_*）。
// 历史遗留的重复索引（idx_daily_bar_ts_code 与主键前缀重复、idx_daily_bar_trade_date
// 与 idx_bar_date 同列重复）不在此列，启动清点会点名它们。
var SchemaIndexes = []string{
	"idx_bar_date", "idx_cal_open_date", "idx_ticket_active", "idx_ticket_date_status", "idx_trace_subject",
}
