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
	// 仓位上限 / 持仓数 / 止损 / 置信度门槛由 risk.DefaultParams 给出固定基准，
	// 不作为配置键暴露；这里只留确实生效、需要人工调整的止盈线。
	{Key: "risk.take_profit_pct", Type: TypeFloat, Default: "0.15", RefuseZero: true},
	// 移动止盈激活线：期间高点 ≥ 成本×(1+arm) 才启用回撤止盈，未激活时下跌只走 8% 止损。
	// 建仓首日 high==cost，旧口径下 5% 回撤线会先于 8% 止损触发，实际把止损收紧成 5%
	// （20261022 600704：成本/高点 5.07、现价 4.81 被"移动止盈"卖出）。
	// 0 合法 = 回到旧口径（成本线即武装）；装配期校验 0..0.5。
	{Key: "risk.trailing_arm_pct", Type: TypeFloat, Default: "0.06"},

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
	// 大盘门槛开关与均线窗口。此前窗口写死为编译期常量 store.MarketMAWindow，
	// 结果是弱势市场里整条买入链停摆、且无任何运行期退路（agent_issues #7）。
	// gate_enabled=false：买入漏斗恒放行、卖出规则 5（大盘恶化）停用——
	// 只开买入关不配卖出，试探仓会在当晚被"排名淘汰/大盘恶化"清光，两者必须同源。
	// gate_ma_window 合法区间 1..60（上限是同步与保留保证的指数深度，见 store.MarketMAWindow）。
	{Key: "screen.gate_enabled", Type: TypeBool, Default: "true"},
	{Key: "screen.gate_ma_window", Type: TypeInt, Default: "60", RefuseZero: true},
	// 弱势试探仓位上限（仅在 gate_enabled=false 且判定为弱势时生效）：
	// 门槛人为放开后若仍按正常风控（单票40%/总仓90%）建仓，弱势市场等于裸奔。
	// 默认总仓 20%、单票 10%（组长 20260924 批准）；装配期校验 ≤ 物理熔断且 single≤total。
	{Key: "screen.gate_off_max_total_pct", Type: TypeFloat, Default: "0.20", RefuseZero: true},
	{Key: "screen.gate_off_max_single_pct", Type: TypeFloat, Default: "0.10", RefuseZero: true},
	// 因子方向模式：reversal = 反向使用动量/低波/流动性（默认）；momentum = 原始方向（保留供 A/B）。
	// 默认取 reversal 的依据是 2023-09~2026-09 的 20 日 RankIC 实测：
	// 原始方向综合分 −0.0896（ICIR −0.648，n=689 个截面），反向 +0.0930；
	// 前后两段独立样本（300 / 389 个截面）一边同为负、另一边同为正，非样本内挑参。
	{Key: "screen.factor_mode", Type: TypeString, Default: "reversal"},
	// ---------- 卖出规则 4（排名淘汰，signal.*）----------
	// 2026-10-14 组长批准（无锡银行/物产中大两笔"买入 2-5 日即被排名淘汰"的复盘）：
	// reversal 的 20 日 RankIC 隐含月度级调仓，而原规则 4 是日频 TopN 即时换仓，
	// 无持有期、无缓冲——实际持有 5/2 个交易日，整笔亏损里手续费占 91%。
	// min_hold_days：建仓后 N 个交易日内排名淘汰不触发；止损/移动止盈/止盈三条
	// 价格型规则不受它保护（只挡"排名换仓"，不挡下跌）。
	// buffer_n：排名保留带 = Top(TopN+N)，跌出保留带才淘汰；买入候选仍只取 TopN，
	// 保留带只喂卖出规则 4（滞回，压掉 TopN 截断线附近的日间抖动）。
	// 0 合法（单独关闭该项保护 = 回到旧口径），负值装配期拒绝。
	{Key: "signal.rank_out_min_hold_days", Type: TypeInt, Default: "10"},
	{Key: "signal.rank_out_buffer_n", Type: TypeInt, Default: "10"},
	// ---------- 弱势期市场退出线（signal.weak_exit_*）----------
	// 只在 screen.gate_enabled=false 时生效：闸门关着就不会有规则 5（大盘恶化），
	// 弱势期的退出只能靠 8% 止损；但阴跌不是一次跌穿止损线，而是每天 -0.5% 磨下去。
	// 触发条件 = 指数收盘 < MA{gate_ma_window} 且 5 日跌幅 ≥ drop_pct；动作只减不清，
	// 卖出 reduce_pct（向下取整到 100 股整手），可减不足一手则跳过（100 股仓位仍只由
	// 止损/移动止盈/止盈管理）。gate=true 时规则 5 原样生效，本组键不参与。
	{Key: "signal.weak_exit_enabled", Type: TypeBool, Default: "true"},
	{Key: "signal.weak_exit_drop_pct", Type: TypeFloat, Default: "0.03"},
	{Key: "signal.weak_exit_reduce_pct", Type: TypeFloat, Default: "0.5"},
	// ---------- 行业集中度上限 ----------
	// 候选侧：因子 TopN 每个行业最多 N 只；0=关闭。买入侧复用同一键：
	// 组合同行业最多 min(2, N) 只（默认 2），弱势档再收紧到 1 只。
	// 依据：弱势行情下同行业个股高度同涨同跌，20261008 的 20 只候选里电子/半导体
	// 占 7 只，分散度是名义上的；候选与持仓两层同时限，才能挡住"换一只同行业票"。
	{Key: "screen.max_per_industry", Type: TypeInt, Default: "3"},
	// ---------- 场内科技 ETF 补口（只在弱势试探期启用）----------
	// 立项依据不是"ETF 更稳"（实测 ETF 日σ 2.29% ≈ 个股中位 2.19%，4 年最大回撤
	// −44%~−61%，它不是防御资产），而是**一手成本**：科技股一手中位 5200 元，
	// 弱势试探的单票上限 1182.68 元把整个科技板块挡在可负担窗口之外（仅 6.5% 命中），
	// 而 ETF 一手 91~160 元。总闸默认关：上线当日零行为变化，开闸由组长择日。
	{Key: "screen.etf_enabled", Type: TypeBool, Default: "false"},
	// 白名单是人工按 fund_basic.name 逐只复核过的资产清单（实测按代码前缀猜会踩空：
	// 515220 是煤炭 ETF、561560 是电力公用事业 ETF），格式 code:name:track，
	// 逗号分隔。名称要进指令单与邮件，跟踪方向要进 LLM 的标的说明——
	// 猜错的名称会让用户按一张错误的单子下单，所以装配期就要求每条带名称。
	// 同赛道只留流动性最好的一只：实测这些标的两两相关最高 0.985，重复没有意义。
	{Key: "screen.etf_whitelist", Type: TypeString, Default: "588000.SH:华夏上证科创板50成份ETF:科创50,588200.SH:嘉实上证科创板芯片ETF:科创芯片,512480.SH:国联安中证全指半导体产品与设备ETF:半导体设备,515050.SH:华夏中证5G通信主题ETF:通信设备,159819.SZ:易方达中证人工智能主题ETF:人工智能,159995.SZ:华夏国证半导体芯片ETF:国证芯片,159852.SZ:嘉实中证软件服务ETF:软件服务"},
	// 流动性下限（窗口内日均成交额，亿元）：1200 元级下单的冲击成本可忽略线。
	// 规模 ≥50 亿不设键（防的是"某天规模塌陷"，不是可调参数；见 dataloader.ETFMinScaleYi）。
	{Key: "screen.etf_min_avg_amount_yi", Type: TypeFloat, Default: "2.0", RefuseZero: true},
	// ---------- LLM（买入决策者，不是可选增强）----------
	// 默认开：关掉它当日不会有任何买单 —— 买什么、买多少由它定，风控只做硬截断。
	{Key: "llm.enabled", Type: TypeBool, Default: "true"},
	{Key: "llm.api_key", Type: TypeSecret, Secret: true},
	{Key: "llm.base_url", Type: TypeString, Default: "https://api.deepseek.com/v1"},
	{Key: "llm.model", Type: TypeString, Default: "deepseek-v4-flash"},
	// 当前没有任何 prompt 挂检索（2026-10-09 实测 /v1/responses 不执行 web_search，
	// 见 internal/llm/prompts.go 顶部），所以这个键暂时不落实际；保留是为了
	// 将来接入真正可用的检索时不用再动装配层与校验。值仍按 low|medium|high 校验。
	{Key: "llm.search_context_size", Type: TypeString, Default: "high"},

	// ---------- 调度（scheduler.BuildJobs 逐键读取）----------
	// 五个触发点：09:00 计划 / 盘中扫描 / 16:30 流水线 / 17:00 待买卖邮件 / 18:00 日报。
	// 盘中扫描窗口（09:30-11:30、13:00-15:00）与保留清理（并入 18:00 日报之后）都不设键。
	{Key: "scheduler.morning", Type: TypeString, Default: "09:00"},
	{Key: "scheduler.pipeline", Type: TypeString, Default: "16:30"},
	{Key: "scheduler.mail_pending", Type: TypeString, Default: "17:00"},
	{Key: "scheduler.report", Type: TypeString, Default: "18:00"},

	// ---------- 保留策略（§3.9）----------
	// daily_bar 的**个股**窗口（自然日）：最深消费者是选股因子窗口 20 个交易日，
	// 45 自然日 ≈ 30 个交易日。大盘指数豁免于本键（store.RetentionRules.ExceptTSCode）——
	// 它的均线窗口要 60 交易日，但每天只新增一根，没必要为它把 5560 只都留长。
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
