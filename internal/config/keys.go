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
	// 监听地址由 serve 子命令的 -addr 决定，不设配置键（避免两处入口互相误导）。
	{Key: "server.api_token", Type: TypeSecret, Secret: true, Required: true},

	// ---------- 账户 ----------
	{Key: "account.initial_capital", Type: TypeInt, Default: "0", WriteOnce: true},
	// 现金锚点：持仓是按券商口径校准进来的（没有成交单支撑），只按"本金 − 成交"推现金
	// 会把这笔持仓成本双算成可用资金。组合同步时把当时的可用现金落成锚点，
	// 此后只把锚点交易日之后成交的净变动加回去。两键都由 sync_portfolio / init 写入。
	{Key: "account.cash_anchor", Type: TypeInt, Default: "0"},
	{Key: "account.cash_anchor_date", Type: TypeString, Default: ""},

	// ---------- 风控 ----------
	// 仓位上限 / 持仓数 / 止损只由 risk.GearTable 按档位给出（Resolve 无条件覆盖），
	// 不作为配置键暴露；这里只留档位无关、确实生效的两个旋钮。
	{Key: "risk.max_sector_pct", Type: TypeFloat, Default: "0.50", RefuseZero: true},
	{Key: "risk.take_profit_pct", Type: TypeFloat, Default: "0.15", RefuseZero: true},

	// ---------- 交易成本 ----------
	{Key: "cost.commission_rate", Type: TypeFloat, Default: "0.00025"},
	{Key: "cost.min_commission", Type: TypeFloat, Default: "5.0"},
	{Key: "cost.stamp_tax_rate", Type: TypeFloat, Default: "0.001"},
	{Key: "cost.transfer_fee_rate", Type: TypeFloat, Default: "0.00001"},

	// ---------- 选股 ----------
	// 单笔最小金额门槛见 risk.DefaultMinSingleAmountFen（由风控参数生效），此处不设重复键；
	// 候选每日重算，无跨日保留期概念。
	{Key: "screen.top_n", Type: TypeInt, Default: "20", RefuseZero: true},
	{Key: "screen.min_circ_mv_w", Type: TypeFloat, Default: "500000", RefuseZero: true},
	{Key: "screen.min_turnover_rate", Type: TypeFloat, Default: "1.0", RefuseZero: true},
	{Key: "screen.price_low", Type: TypeFloat, Default: "2.0", RefuseZero: true},
	{Key: "screen.pe_ttm_max", Type: TypeFloat, Default: "80.0", RefuseZero: true},
	{Key: "screen.pb_max", Type: TypeFloat, Default: "10.0", RefuseZero: true},
	{Key: "screen.min_list_days", Type: TypeInt, Default: "60"},
	{Key: "screen.sector_top_k", Type: TypeInt, Default: "8", RefuseZero: true},
	{Key: "screen.min_sector_members", Type: TypeInt, Default: "30", RefuseZero: true},
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

	// ---------- LLM（买入决策者，不是可选增强）----------
	// 默认开：关掉它当日不会有任何买单 —— 买什么、买多少由它定，风控只做硬截断。
	{Key: "llm.enabled", Type: TypeBool, Default: "true"},
	{Key: "llm.api_key", Type: TypeSecret, Secret: true},
	{Key: "llm.base_url", Type: TypeString, Default: "https://api.deepseek.com/v1"},
	{Key: "llm.model", Type: TypeString, Default: "deepseek-v4-flash"},
	// 只对挂了 web_search 的消息面维度有意义。以前"问不出结果就降档重试"是误诊：
	// 真因是请求没传 max_output_tokens（见 llm.maxOutputTokens），预算修好后一档到底。
	{Key: "llm.search_context_size", Type: TypeString, Default: "high"},

	// ---------- 调度（scheduler.BuildJobs 逐键读取）----------
	// 五个触发点：09:00 计划 / 盘中扫描 / 16:30 流水线 / 17:00 待买卖邮件 / 18:00 日报。
	// 盘中扫描窗口（09:30-11:30、13:00-15:00）与保留清理（并入 18:00 日报之后）都不设键。
	{Key: "scheduler.morning", Type: TypeString, Default: "09:00"},
	{Key: "scheduler.pipeline", Type: TypeString, Default: "16:30"},
	{Key: "scheduler.mail_pending", Type: TypeString, Default: "17:00"},
	{Key: "scheduler.report", Type: TypeString, Default: "18:00"},

	// ---------- 保留策略（§3.9）----------
	{Key: "retention.bar_days", Type: TypeInt, Default: "45"},
	// 停牌集合（config_kv 的 suspend:<日期> 一行）只在选股当日读一次，无回测/复算路径，
	// 留 3 天只是给跨天重跑留余量。
	{Key: "retention.suspend_days", Type: TypeInt, Default: "3"},
	// 任务、告警、发信已并成一张 run_trace，共用一个窗口。
	{Key: "retention.trace_days", Type: TypeInt, Default: "90"},

	// ---------- 通知收件人（逗号分隔；M1~M5 与告警邮件共用）----------
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
