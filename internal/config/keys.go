// Package config config_kv 读写、默认值目录、类型化解析、危险零值拒绝、凭据掩码。
//
// 依赖方向：config 依赖 store（读取 config_kv 表），不依赖其他业务包（§1.2）。
// 配置全部存 SQLite（用户要求单一数据源），不用 viper（§9.2）。
package config

import "sort"

// KeyType 配置值类型。
type KeyType string

const (
	TypeString KeyType = "string"
	TypeInt    KeyType = "int"
	TypeFloat  KeyType = "float"
	TypeBool   KeyType = "bool"
	TypeSecret KeyType = "secret"
)

// KeySpec 单个配置键的规范：键名、类型、默认值、是否凭据、是否必配、是否拒绝零值、是否 write-once。
// 这是全系统配置键的唯一定义处（ARCHITECTURE §2.5）。
type KeySpec struct {
	Key        string
	Type       KeyType
	Default    string // 默认值的原始字符串
	Secret     bool   // 凭据：输出通道默认掩码
	Required   bool   // 必配：空值拒绝启动
	RefuseZero bool   // 数值为 0 视为危险零值，拒绝启动
	WriteOnce  bool   // 首次写入后不再自动更新（如本金）
}

// KeySpecs 完整配置键目录（ARCHITECTURE §3.1 + PRD §8.3）。
// 注意：RefuseZero 键必须有非零默认值，否则全新库无法通过启动自检。
var KeySpecs = []KeySpec{
	// ---------- Tushare ----------
	{Key: "tushare.token", Type: TypeSecret, Secret: true, Required: true},
	{Key: "tushare.base_url", Type: TypeString, Default: "http://api.tushare.pro"},
	{Key: "tushare.rate_per_min", Type: TypeInt, Default: "200", RefuseZero: true},

	// ---------- 邮件 ----------
	{Key: "mail.enabled", Type: TypeBool, Default: "false"},
	{Key: "mail.from", Type: TypeString},
	{Key: "mail.password", Type: TypeSecret, Secret: true},
	{Key: "mail.smtp_host", Type: TypeString, Default: "smtp.qq.com"},
	{Key: "mail.smtp_port", Type: TypeInt, Default: "465", RefuseZero: true},

	// ---------- 服务 / 鉴权 ----------
	{Key: "server.host", Type: TypeString, Default: "127.0.0.1"},
	{Key: "server.port", Type: TypeInt, Default: "8787"},
	{Key: "server.api_token", Type: TypeSecret, Secret: true, Required: true},

	// ---------- 账户 ----------
	{Key: "account.initial_capital", Type: TypeInt, Default: "0", WriteOnce: true},
	{Key: "account.cash", Type: TypeInt, Default: "0"},

	// ---------- 风控 ----------
	{Key: "risk.max_total_position_pct", Type: TypeFloat, Default: "0.90", RefuseZero: true},
	{Key: "risk.max_position_pct", Type: TypeFloat, Default: "0.40", RefuseZero: true},
	{Key: "risk.max_sector_pct", Type: TypeFloat, Default: "0.50", RefuseZero: true},
	{Key: "risk.max_positions", Type: TypeInt, Default: "0"},
	{Key: "risk.stop_loss_pct", Type: TypeFloat, Default: "0.08", RefuseZero: true},
	{Key: "risk.trailing_stop_pct", Type: TypeFloat, Default: "0.05", RefuseZero: true},
	{Key: "risk.take_profit_pct", Type: TypeFloat, Default: "0.15", RefuseZero: true},

	// ---------- 交易成本 ----------
	{Key: "cost.commission_rate", Type: TypeFloat, Default: "0.00025"},
	{Key: "cost.min_commission", Type: TypeFloat, Default: "5.0"},
	{Key: "cost.stamp_tax_rate", Type: TypeFloat, Default: "0.001"},
	{Key: "cost.transfer_fee_rate", Type: TypeFloat, Default: "0.00001"},

	// ---------- 交易 ----------
	{Key: "trading.min_trade_amount", Type: TypeInt, Default: "3000"},

	// ---------- 选股 ----------
	{Key: "screen.enabled", Type: TypeBool, Default: "true"},
	{Key: "screen.top_n", Type: TypeInt, Default: "20", RefuseZero: true},
	{Key: "screen.candidate_keep_days", Type: TypeInt, Default: "10", RefuseZero: true},
	{Key: "screen.min_circ_mv_w", Type: TypeFloat, Default: "500000", RefuseZero: true},
	{Key: "screen.min_turnover_rate", Type: TypeFloat, Default: "1.0", RefuseZero: true},
	{Key: "screen.price_low", Type: TypeFloat, Default: "2.0", RefuseZero: true},
	{Key: "screen.price_high", Type: TypeFloat, Default: "100.0", RefuseZero: true},
	{Key: "screen.pe_ttm_max", Type: TypeFloat, Default: "80.0", RefuseZero: true},
	{Key: "screen.pb_max", Type: TypeFloat, Default: "10.0", RefuseZero: true},
	{Key: "screen.min_bar_rows", Type: TypeInt, Default: "5000"},

	// ---------- 季度目标 ----------
	{Key: "goal.quarterly_target_pct", Type: TypeFloat, Default: "0.15", RefuseZero: true},
	{Key: "goal.max_drawdown_budget", Type: TypeFloat, Default: "0.10", RefuseZero: true},
	{Key: "goal.tighten_at_budget", Type: TypeFloat, Default: "0.70"},
	{Key: "goal.defend_at_budget", Type: TypeFloat, Default: "1.00"},
	{Key: "goal.upgrade_hysteresis", Type: TypeFloat, Default: "0.15", RefuseZero: true},
	{Key: "goal.upgrade_days", Type: TypeInt, Default: "3", RefuseZero: true},
	{Key: "goal.pace_policy", Type: TypeString, Default: "unrestricted"},
	{Key: "goal.pace_max_boost_pct", Type: TypeFloat, Default: "0.10"},
	{Key: "goal.pace_allow_if_budget_below", Type: TypeFloat, Default: "0.30"},
	{Key: "goal.lock_at_progress", Type: TypeFloat, Default: "1.00", RefuseZero: true},
	{Key: "goal.lock_budget_below", Type: TypeFloat, Default: "0.70", RefuseZero: true},

	// ---------- LLM ----------
	{Key: "llm.enabled", Type: TypeBool, Default: "false"},
	{Key: "llm.api_key", Type: TypeSecret, Secret: true},
	{Key: "llm.base_url", Type: TypeString, Default: "https://api.deepseek.com/v1"},
	{Key: "llm.model", Type: TypeString, Default: "deepseek-chat"},

	// ---------- 调度（PRD §5.2 时间线）----------
	{Key: "scheduler.daily_check", Type: TypeString, Default: "07:00"},
	{Key: "scheduler.premarket", Type: TypeString, Default: "08:30"},
	{Key: "scheduler.t1_settle", Type: TypeString, Default: "09:25"},
	{Key: "scheduler.data_sync", Type: TypeString, Default: "15:05"},
	{Key: "scheduler.data_retry", Type: TypeString, Default: "15:25,15:50,16:30"},
	{Key: "scheduler.snapshot", Type: TypeString, Default: "15:20"},
	{Key: "scheduler.screener", Type: TypeString, Default: "15:30"},
	{Key: "scheduler.signal", Type: TypeString, Default: "15:50"},
	{Key: "scheduler.ticket_mail", Type: TypeString, Default: "16:10"},
	{Key: "scheduler.cleanup", Type: TypeString, Default: "16:40"},
	{Key: "scheduler.daily_report", Type: TypeString, Default: "20:00"},

	// ---------- 保留策略（§3.9）----------
	{Key: "retention.bar_years", Type: TypeInt, Default: "3"},
	{Key: "retention.fina_quarters", Type: TypeInt, Default: "8"},
	{Key: "retention.mf_days", Type: TypeInt, Default: "60"},
	{Key: "retention.screen_days", Type: TypeInt, Default: "90"},
	{Key: "retention.signal_days", Type: TypeInt, Default: "365"},
	{Key: "retention.alert_days", Type: TypeInt, Default: "180"},
	{Key: "retention.job_days", Type: TypeInt, Default: "90"},
	{Key: "retention.mail_days", Type: TypeInt, Default: "30"},
	{Key: "retention.llm_days", Type: TypeInt, Default: "90"},
	{Key: "retention.log_days", Type: TypeInt, Default: "30"},

	// ---------- 看门狗 ----------
	{Key: "watch.mail_to", Type: TypeString},
}

// specIndex 构建键名索引（用于快速查找与稳定遍历）。
var specIndex = func() map[string]KeySpec {
	m := make(map[string]KeySpec, len(KeySpecs))
	for _, s := range KeySpecs {
		m[s.Key] = s
	}
	return m
}()

// SortedKeys 返回按字典序排列的键名（用于 dump 稳定输出）。
func SortedKeys() []string {
	keys := make([]string, 0, len(KeySpecs))
	for _, s := range KeySpecs {
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)
	return keys
}

// FindSpec 查找键规范。
func FindSpec(key string) (KeySpec, bool) {
	s, ok := specIndex[key]
	return s, ok
}
