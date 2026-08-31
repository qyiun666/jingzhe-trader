package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/viper"
)

// Config 全局配置
// 注意: 不含 database.path —— 配置本体就存在该库内, 路径不可能由配置给出 (自指),
// 统一由 -db flag / JZ_DB_PATH 决定, 见 appcfg.ResolveDBPath
type Config struct {
	Tushare    TushareConfig    `mapstructure:"tushare"`
	Cost       CostConfig       `mapstructure:"cost"`
	Backtest   BacktestConfig   `mapstructure:"backtest"`
	Risk       RiskConfig       `mapstructure:"risk"`
	Log        LogConfig        `mapstructure:"log"`
	Broker     BrokerConfig     `mapstructure:"broker"`
	Strategy   StrategyConfig   `mapstructure:"strategy"`
	Universe   UniverseConfig   `mapstructure:"universe"`
	Server     ServerConfig     `mapstructure:"server"`
	Mail       MailConfig       `mapstructure:"mail"`
	LLM        LLMConfig        `mapstructure:"llm"`
	Dataloader DataloaderConfig `mapstructure:"dataloader"`
	Scheduler  SchedulerConfig  `mapstructure:"scheduler"`
	Trading    TradingConfig    `mapstructure:"trading"`
	Retention  RetentionConfig  `mapstructure:"retention"`
	Screener   ScreenerConfig   `mapstructure:"screener"`
	Goal       GoalConfig       `mapstructure:"goal"`
}

// GoalConfig 季度目标跟踪配置
// 系统按日历季度跟踪收益目标进度与回撤预算, 超预算时自动收紧风险敞口
type GoalConfig struct {
	Enabled            bool    `mapstructure:"enabled"`              // 是否启用目标跟踪
	QuarterlyTargetPct float64 `mapstructure:"quarterly_target_pct"` // 季度收益目标 (如 0.15=15%)
	MaxDrawdownBudget  float64 `mapstructure:"max_drawdown_budget"`  // 季度内最大回撤预算 (如 0.10=10%)
	AutoAdjust         bool    `mapstructure:"auto_adjust"`          // 是否自动调节风险敞口 (false=仅跟踪告警)
}

// SchedulerConfig 内置调度器配置
type SchedulerConfig struct {
	Enabled          bool           `mapstructure:"enabled"`            // 是否启用调度器
	PremarketTime    string         `mapstructure:"premarket_time"`     // 盘前总结时间 HH:MM
	DataUpdateTime   string         `mapstructure:"data_update_time"`   // 数据更新时间 HH:MM
	ScreenerTime     string         `mapstructure:"screener_time"`      // 自动选股时间 HH:MM
	SignalTime       string         `mapstructure:"signal_time"`        // EOD信号生成时间 HH:MM
	ReportTime       string         `mapstructure:"report_time"`        // 日报生成时间 HH:MM
	DebateReviewTime string         `mapstructure:"debate_review_time"` // 辩论复盘回填时间 HH:MM
	Intraday         IntradayConfig `mapstructure:"intraday"`           // 盘中监控
}

// IntradayConfig 盘中止损监控配置
type IntradayConfig struct {
	Enabled     bool   `mapstructure:"enabled"`      // 是否启用盘中监控
	IntervalMin int    `mapstructure:"interval_min"` // 监控间隔(分钟)
	Start       string `mapstructure:"start"`        // 监控开始时间 HH:MM
	End         string `mapstructure:"end"`          // 监控结束时间 HH:MM
}

// TradingConfig 交易执行配置
type TradingConfig struct {
	AutoExecute    bool    `mapstructure:"auto_execute"`     // true=确认后自动下单, false=仅生成计划
	MinTradeAmount float64 `mapstructure:"min_trade_amount"` // 最小单笔交易金额, 0=按佣金自适应
	MaxPositions   int     `mapstructure:"max_positions"`    // 最大持仓数, 0=按资金自适应
}

// RetentionConfig 数据保留/自动清理配置
type RetentionConfig struct {
	BarYears     int    `mapstructure:"bar_years"`     // 行情数据保留年数
	NewsDays     int    `mapstructure:"news_days"`     // 新闻保留天数
	PlanDays     int    `mapstructure:"plan_days"`     // 交易计划/任务记录保留天数
	ActionDays   int    `mapstructure:"action_days"`   // 动作日志(action_log)保留天数 (每任务每笔都写, 增长最快)
	AlertDays    int    `mapstructure:"alert_days"`    // 告警(agent_alert)保留天数
	ScreenDays   int    `mapstructure:"screen_days"`   // 选股结果(screen_result)保留天数
	DebateDays   int    `mapstructure:"debate_days"`   // 辩论与复盘记录保留天数
	FinaQuarters int    `mapstructure:"fina_quarters"` // 财务指标保留最近N个报告期
	BacktestRuns int    `mapstructure:"backtest_runs"` // 保留最近N个回测run
	LogDays      int    `mapstructure:"log_days"`      // 日志文件保留天数
	ReportFiles  int    `mapstructure:"report_files"`  // 保留最近N个报告文件
	CleanupTime  string `mapstructure:"cleanup_time"`  // 每日清理时间 HH:MM
}

// ScreenerConfig 自动选股器配置
type ScreenerConfig struct {
	Enabled         bool     `mapstructure:"enabled"`           // 是否启用选股器
	MaxCandidates   int      `mapstructure:"max_candidates"`    // 最大候选股票数
	ExcludeCodes    []string `mapstructure:"exclude_codes"`     // 排除的股票代码 (配置池已有)
	ExcludeST       bool     `mapstructure:"exclude_st"`        // 排除ST股
	MinListDays     int      `mapstructure:"min_list_days"`     // 最小上市天数 (排除新股)
	MinPrice        float64  `mapstructure:"min_price"`         // 最低股价
	MaxPrice        float64  `mapstructure:"max_price"`         // 最高股价
	MinTurnoverRate float64  `mapstructure:"min_turnover_rate"` // 最低换手率 %
	MaxPE           float64  `mapstructure:"max_pe"`            // 最大PE_TTM
	MaxPB           float64  `mapstructure:"max_pb"`            // 最大PB
	MinCircMV       float64  `mapstructure:"min_circ_mv"`       // 最小流通市值 (万元)
	MaxCircMV       float64  `mapstructure:"max_circ_mv"`       // 最大流通市值 (万元)
}

// DataloaderConfig 数据加载器配置
type DataloaderConfig struct {
	FilterMode    bool     `mapstructure:"filter_mode"`    // 筛选模式: 只拉取股票池+持仓+关注列表的股票
	Watchlist     []string `mapstructure:"watchlist"`      // 额外关注的股票代码列表
	EnableLimit   bool     `mapstructure:"enable_limit"`   // 是否同步涨跌停价
	EnableBasic   bool     `mapstructure:"enable_basic"`   // 是否同步每日基本面
	EnableFund    bool     `mapstructure:"enable_fund"`    // 是否同步ETF/基金日线
	EnableCleanup bool     `mapstructure:"enable_cleanup"` // 是否允许清理非关注股票数据(危险操作, 默认关闭)
	SyncOptional  bool     `mapstructure:"sync_optional"`  // server每日更新是否同步可选数据(新闻/资金流/龙虎榜/财务指标)
}

// LLMConfig LLM 配置
// 用于新闻深度分析和选股辅助，不直接做交易决策
// 默认关闭，不影响系统核心功能
type LLMConfig struct {
	Enabled        bool    `mapstructure:"enabled"`         // 是否启用 LLM
	APIKey         string  `mapstructure:"api_key"`         // API Key
	BaseURL        string  `mapstructure:"base_url"`        // API 地址，默认 DeepSeek
	Model          string  `mapstructure:"model"`           // 模型名称，默认 deepseek-chat
	Temperature    float64 `mapstructure:"temperature"`     // 采样温度, 默认 0.3
	MaxTokens      int     `mapstructure:"max_tokens"`      // 输出上限, 默认 2048
	TimeoutSeconds int     `mapstructure:"timeout_seconds"` // HTTP 超时秒数, 默认 30
	JSONMode       bool    `mapstructure:"json_mode"`       // 强制 JSON 输出 (response_format=json_object), 仅 DeepSeek 支持
	MaxConcurrency int     `mapstructure:"max_concurrency"` // 并发在飞请求上限, 默认 3
	RPS            float64 `mapstructure:"rps"`             // 每秒请求数上限 (0=不限速), 默认 2
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Host           string   `mapstructure:"host"`            // 监听地址
	Port           int      `mapstructure:"port"`            // 监听端口
	APIToken       string   `mapstructure:"api_token"`       // API鉴权token, 非空时启用Bearer校验
	AllowedOrigins []string `mapstructure:"allowed_origins"` // CORS允许的来源列表
}

// MailConfig 邮件通知配置 (QQ 邮箱, smtp.qq.com:465)
// Password 为 SMTP 授权码; 随整棵配置存于 config_kv, 环境变量 JZ_MAIL_PASSWORD 非空时应急覆盖
type MailConfig struct {
	Enabled  bool   `mapstructure:"enabled"`  // 是否启用邮件通知
	From     string `mapstructure:"from"`     // 发件邮箱 (即收件人)
	Password string `mapstructure:"password"` // SMTP 授权码
}

type TushareConfig struct {
	Token         string `mapstructure:"token"`
	BaseURL       string `mapstructure:"base_url"`
	RateLimit     int    `mapstructure:"rate_limit"`
	MaxRetries    int    `mapstructure:"max_retries"`
	RetryInterval int    `mapstructure:"retry_interval"`
}

type CostConfig struct {
	CommissionRate  float64 `mapstructure:"commission_rate"`
	MinCommission   float64 `mapstructure:"min_commission"`
	StampTaxRate    float64 `mapstructure:"stamp_tax_rate"`
	TransferFeeRate float64 `mapstructure:"transfer_fee_rate"`
}

type BacktestConfig struct {
	StartDate      string  `mapstructure:"start_date"`
	EndDate        string  `mapstructure:"end_date"`
	InitialCapital float64 `mapstructure:"initial_capital"`
	Benchmark      string  `mapstructure:"benchmark"`
	Slippage       float64 `mapstructure:"slippage"`
	FillPrice      string  `mapstructure:"fill_price"`
}

type RiskConfig struct {
	MaxPositionPct      float64 `mapstructure:"max_position_pct"`
	MaxTotalPositionPct float64 `mapstructure:"max_total_position_pct"`
	MaxSectorPct        float64 `mapstructure:"max_sector_pct"`
	StopLossPct         float64 `mapstructure:"stop_loss_pct"`
	TakeProfitPct       float64 `mapstructure:"take_profit_pct"`
	TrailingStopPct     float64 `mapstructure:"trailing_stop_pct"` // 移动止盈回撤比例 (0=不启用; 启用后盈利达止盈线改按高点回撤退出)
	ExcludeST           bool    `mapstructure:"exclude_st"`
	MinListDays         int     `mapstructure:"min_list_days"`
}

type LogConfig struct {
	Level    string `mapstructure:"level"`
	Format   string `mapstructure:"format"`
	Output   string `mapstructure:"output"`
	FilePath string `mapstructure:"file_path"`
}

// BrokerConfig 券商配置
type BrokerConfig struct {
	Type string    `mapstructure:"type"` // paper / qmt
	QMT  QMTConfig `mapstructure:"qmt"`
}

// QMTConfig miniQMT 配置
type QMTConfig struct {
	URL       string `mapstructure:"url"`
	Path      string `mapstructure:"path"`
	AccountID string `mapstructure:"account_id"`
	SessionID int    `mapstructure:"session_id"`
}

// StrategyConfig 策略配置
type StrategyConfig struct {
	MACross     MACrossConfig     `mapstructure:"ma_cross"`
	MACD        MACDConfig        `mapstructure:"macd"`
	MultiFactor MultiFactorConfig `mapstructure:"multi_factor"`
}

// MACrossConfig 均线交叉策略配置
type MACrossConfig struct {
	ShortPeriod    int     `mapstructure:"short_period"`
	LongPeriod     int     `mapstructure:"long_period"`
	PositionPct    float64 `mapstructure:"position_pct"`
	EnableAdaptive bool    `mapstructure:"enable_adaptive"`
	// 信号过滤器阈值 (0=用策略内置默认值; 金叉信号需通过趋势强度+量能确认, 过严会导致长期无交易)
	VolConfirmRatio float64 `mapstructure:"vol_confirm_ratio"` // 量能确认倍数, 默认1.2 (当日量>过去5日均量×倍数)
	TrendThreshold  float64 `mapstructure:"trend_threshold"`   // 趋势强度阈值(小数), 默认0.005 (金叉日短均线须高于长均线0.5%%)
}

// MACDConfig MACD策略配置
type MACDConfig struct {
	Fast           int     `mapstructure:"fast"`
	Slow           int     `mapstructure:"slow"`
	Signal         int     `mapstructure:"signal"`
	PositionPct    float64 `mapstructure:"position_pct"`
	EnableAdaptive bool    `mapstructure:"enable_adaptive"`
}

// MultiFactorConfig 多因子策略配置
type MultiFactorConfig struct {
	TopN           int     `mapstructure:"top_n"`
	RebalanceFreq  string  `mapstructure:"rebalance_freq"`
	PositionPct    float64 `mapstructure:"position_pct"`
	StopLossPct    float64 `mapstructure:"stop_loss_pct"`
	TakeProfitPct  float64 `mapstructure:"take_profit_pct"`
	EnableAdaptive bool    `mapstructure:"enable_adaptive"`
}

// UniverseConfig 股票池配置
type UniverseConfig struct {
	Bluechip string `mapstructure:"bluechip"`
	Tech     string `mapstructure:"tech"`
}

// ParseUniverseCSV 解析命令行传入的股票池 CSV (逗号分隔, 去空白), 空串返回 nil
// cmd/backtest 与 cmd/optimizer 共用, 避免各写一份
func ParseUniverseCSV(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// UniverseCodes 返回配置股票池代码列表 (bluechip + tech 去重, 保持顺序)
func (c *Config) UniverseCodes() []string {
	seen := make(map[string]bool)
	var codes []string
	for _, group := range []string{c.Universe.Bluechip, c.Universe.Tech} {
		for _, code := range strings.Split(group, ",") {
			code = strings.TrimSpace(code)
			if code == "" || seen[code] {
				continue
			}
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return codes
}

// StrategyParams 返回策略初始化参数 (供 Strategy.Init, 回测/服务/调度器共用同一入口)
func (c *Config) StrategyParams(name string) map[string]interface{} {
	params := make(map[string]interface{})
	switch name {
	case "ma_cross":
		params["short_period"] = float64(c.Strategy.MACross.ShortPeriod)
		params["long_period"] = float64(c.Strategy.MACross.LongPeriod)
		params["position_pct"] = c.Strategy.MACross.PositionPct
		params["enable_adaptive"] = c.Strategy.MACross.EnableAdaptive
		// 过滤器阈值: 仅显式配置(>0)时覆盖内置默认, 避免零值误覆盖
		if c.Strategy.MACross.VolConfirmRatio > 0 {
			params["vol_confirm_ratio"] = c.Strategy.MACross.VolConfirmRatio
		}
		if c.Strategy.MACross.TrendThreshold > 0 {
			params["trend_threshold"] = c.Strategy.MACross.TrendThreshold
		}
	case "macd":
		params["fast"] = float64(c.Strategy.MACD.Fast)
		params["slow"] = float64(c.Strategy.MACD.Slow)
		params["signal"] = float64(c.Strategy.MACD.Signal)
		params["position_pct"] = c.Strategy.MACD.PositionPct
		params["enable_adaptive"] = c.Strategy.MACD.EnableAdaptive
	case "multi_factor":
		// 策略 Init 读这几个键, 必须全部透传: 此前只传 position_pct,
		// 其余配置项写了也不生效
		params["top_n"] = float64(c.Strategy.MultiFactor.TopN)
		params["position_pct"] = c.Strategy.MultiFactor.PositionPct
		params["stop_loss_pct"] = c.Strategy.MultiFactor.StopLossPct
		params["take_profit_pct"] = c.Strategy.MultiFactor.TakeProfitPct
		params["enable_adaptive"] = c.Strategy.MultiFactor.EnableAdaptive
		params["rebalance_freq"] = c.Strategy.MultiFactor.RebalanceFreq
	default:
		params["position_pct"] = 0.15
	}
	return params
}

// newViper 构造装载了全部默认值的 viper 实例 (默认值目录的唯一来源)
// 基线类型固定 json: viper 从字节流装载时必须预先知道类型, 运行期来源就是库里的 JSON 文档
func newViper() *viper.Viper {
	v := viper.New()
	v.SetConfigType("json")
	v.AutomaticEnv()

	// 设置默认值
	v.SetDefault("tushare.base_url", "http://api.tushare.pro")
	v.SetDefault("tushare.rate_limit", 450)
	v.SetDefault("tushare.max_retries", 3)
	v.SetDefault("tushare.retry_interval", 2)
	v.SetDefault("cost.commission_rate", 0.000085)
	v.SetDefault("cost.min_commission", 5.0)
	v.SetDefault("cost.stamp_tax_rate", 0.0005)
	v.SetDefault("cost.transfer_fee_rate", 0.00001)
	v.SetDefault("backtest.initial_capital", 1000000)
	v.SetDefault("backtest.benchmark", "000300.SH")
	v.SetDefault("backtest.slippage", 0.0002)
	v.SetDefault("backtest.fill_price", "next_open")
	v.SetDefault("log.level", "info")
	v.SetDefault("log.format", "console")
	v.SetDefault("log.output", "stdout")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.host", "127.0.0.1")
	v.SetDefault("server.allowed_origins", []string{"http://localhost", "http://127.0.0.1"})
	v.SetDefault("mail.enabled", false)
	v.SetDefault("llm.enabled", false)
	v.SetDefault("llm.base_url", "https://api.deepseek.com/v1")
	v.SetDefault("llm.model", "deepseek-chat")
	v.SetDefault("llm.temperature", 0.3)
	v.SetDefault("llm.max_tokens", 2048)
	v.SetDefault("llm.timeout_seconds", 30)
	v.SetDefault("llm.json_mode", true) // 实测 DeepSeek 支持; 全部内置 prompt 均含「JSON」字样, 满足前置条件
	v.SetDefault("llm.max_concurrency", 3)
	v.SetDefault("llm.rps", 2) // DeepSeek 免费档保守值, 0 表示不限速
	v.SetDefault("dataloader.filter_mode", false)
	v.SetDefault("dataloader.enable_limit", true)
	v.SetDefault("dataloader.enable_basic", true)
	v.SetDefault("dataloader.enable_fund", true)
	v.SetDefault("dataloader.enable_cleanup", false)
	v.SetDefault("dataloader.sync_optional", true)
	v.SetDefault("scheduler.enabled", true)
	v.SetDefault("scheduler.premarket_time", "09:00")
	v.SetDefault("scheduler.data_update_time", "15:10")
	v.SetDefault("scheduler.screener_time", "15:15")
	v.SetDefault("scheduler.signal_time", "18:00")
	v.SetDefault("scheduler.report_time", "18:00")
	v.SetDefault("scheduler.debate_review_time", "15:20")
	v.SetDefault("scheduler.intraday.enabled", true)
	v.SetDefault("scheduler.intraday.interval_min", 5)
	v.SetDefault("scheduler.intraday.start", "09:30")
	v.SetDefault("scheduler.intraday.end", "15:00")
	v.SetDefault("trading.auto_execute", false)
	v.SetDefault("trading.min_trade_amount", 0)
	v.SetDefault("trading.max_positions", 0)
	v.SetDefault("retention.bar_years", 3)
	v.SetDefault("retention.news_days", 30)
	v.SetDefault("retention.plan_days", 90)
	v.SetDefault("retention.action_days", 90)
	v.SetDefault("retention.alert_days", 90)
	v.SetDefault("retention.screen_days", 90)
	v.SetDefault("retention.debate_days", 180)
	v.SetDefault("retention.fina_quarters", 8)
	v.SetDefault("retention.backtest_runs", 20)
	v.SetDefault("retention.log_days", 30)
	v.SetDefault("retention.report_files", 30)
	v.SetDefault("retention.cleanup_time", "16:30")
	v.SetDefault("screener.enabled", false)
	v.SetDefault("screener.max_candidates", 20)
	v.SetDefault("screener.exclude_st", true)
	v.SetDefault("screener.min_list_days", 60)
	v.SetDefault("screener.min_price", 2.0)
	v.SetDefault("screener.max_price", 100.0)
	v.SetDefault("screener.min_turnover_rate", 1.0)
	v.SetDefault("screener.max_pe", 80.0)
	v.SetDefault("screener.max_pb", 10.0)
	v.SetDefault("screener.min_circ_mv", 50000.0)
	v.SetDefault("screener.max_circ_mv", 0.0)

	// 以下段落此前只存在于配置文件、没有代码默认值。配置源改成 SQLite 后,
	// 空库种子出的这些键会落到 Go 零值, 而零值在这套系统里不是"未配置"而是"危险配置":
	// risk.max_position_pct=0 会让风控把每笔买入裁成 0 股, ma_cross.short_period=0 会让均线计算失效。
	// 取值沿用 README 承诺的 1 万元档画像。凭据键 (tushare.token / llm.api_key) 故意不给默认值。
	v.SetDefault("goal.enabled", true)
	v.SetDefault("goal.quarterly_target_pct", 0.15)
	v.SetDefault("goal.max_drawdown_budget", 0.10)
	v.SetDefault("goal.auto_adjust", true)
	v.SetDefault("risk.max_position_pct", 0.40)
	v.SetDefault("risk.max_total_position_pct", 0.90)
	v.SetDefault("risk.max_sector_pct", 0.60)
	v.SetDefault("risk.stop_loss_pct", 0.08)
	v.SetDefault("risk.take_profit_pct", 0.15)
	v.SetDefault("risk.trailing_stop_pct", 0.05)
	v.SetDefault("risk.exclude_st", true)
	v.SetDefault("risk.min_list_days", 60)
	v.SetDefault("strategy.ma_cross.short_period", 3)
	v.SetDefault("strategy.ma_cross.long_period", 25)
	v.SetDefault("strategy.ma_cross.position_pct", 0.50)
	v.SetDefault("strategy.ma_cross.enable_adaptive", false)
	v.SetDefault("strategy.ma_cross.vol_confirm_ratio", 1.05)
	v.SetDefault("strategy.ma_cross.trend_threshold", 0.001)
	v.SetDefault("strategy.macd.fast", 12)
	v.SetDefault("strategy.macd.slow", 26)
	v.SetDefault("strategy.macd.signal", 9)
	v.SetDefault("strategy.macd.position_pct", 0.30)
	v.SetDefault("strategy.macd.enable_adaptive", true)
	v.SetDefault("strategy.multi_factor.top_n", 3)
	v.SetDefault("strategy.multi_factor.rebalance_freq", "weekly")
	v.SetDefault("strategy.multi_factor.position_pct", 0.30)
	v.SetDefault("strategy.multi_factor.stop_loss_pct", 0.08)
	v.SetDefault("strategy.multi_factor.take_profit_pct", 0.15)
	v.SetDefault("strategy.multi_factor.enable_adaptive", false)
	v.SetDefault("strategy.intraday_t.t_amount_pct", 0.40)
	v.SetDefault("strategy.intraday_t.profit_target", 0.012)
	v.SetDefault("strategy.intraday_t.stop_loss_pct", -0.005)
	v.SetDefault("strategy.intraday_t.lookback_days", 20)
	v.SetDefault("broker.type", "paper")
	v.SetDefault("broker.qmt.url", "http://127.0.0.1:16888")
	v.SetDefault("broker.qmt.path", "")
	v.SetDefault("broker.qmt.account_id", "")
	v.SetDefault("broker.qmt.session_id", 123456)
	v.SetDefault("screener.exclude_codes", []string{})
	v.SetDefault("backtest.start_date", "20240101")
	v.SetDefault("backtest.end_date", "20261231")
	// 沪深300: 大盘过滤策略与辩论的大盘分析师都依赖指数日线, 空 watchlist 会让大盘分析恒为"数据缺失"
	v.SetDefault("dataloader.watchlist", []string{"000300.SH"})
	v.SetDefault("log.file_path", "logs/chaogu.log")
	v.SetDefault("screener.enabled", true)
	v.SetDefault("universe.bluechip", "")
	v.SetDefault("universe.tech", "")
	v.SetDefault("server.api_token", "")
	v.SetDefault("mail.from", "")
	return v
}

// LoadFile 从 YAML/JSON 文件装载配置
// 仅供一次性 `cmd/config import` 把旧 config.yaml 搬进 SQLite 时使用, 运行期配置一律走 LoadFromJSON
func LoadFile(path string) (*Config, error) {
	v := newViper()
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		v.SetConfigType("json")
	} else {
		v.SetConfigType("yaml")
	}
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	return resolve(v)
}

// readDoc 把库内配置文档读入 viper; 空文档表示尚未种子, 此时全部键取代码默认值
// (全新库第一次 dump/get 就会走到这个分支, 直接喂给 viper 会报 unexpected end of JSON input)
func readDoc(v *viper.Viper, doc []byte) error {
	if len(doc) == 0 {
		return nil
	}
	if err := v.ReadConfig(bytes.NewReader(doc)); err != nil {
		return fmt.Errorf("解析配置文档失败: %w", err)
	}
	return nil
}

// LoadFromJSON 从 JSON 文档装载配置 (运行期唯一入口, 数据源为 config_kv 的 config 行)
func LoadFromJSON(data []byte) (*Config, error) {
	v := newViper()
	if err := readDoc(v, data); err != nil {
		return nil, err
	}
	return resolve(v)
}

// resolve 反序列化 + 环境变量应急覆盖 (非空才生效, 便于故障时不落库直接顶掉密钥)
func resolve(v *viper.Viper) (*Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	applyEnvOverrides(&cfg)
	return &cfg, nil
}

// DefaultJSON 输出全默认值 JSON 文档, 供首次种子写入 config_kv
func DefaultJSON() ([]byte, error) {
	return dumpJSON(newViper())
}

// EffectiveJSON 输出配置文档合并默认值后的"生效值", 凭据字段掩码
// 供 cmd/config dump 与旧 YAML 逐段比对, 确认搬运无丢项
func EffectiveJSON(data []byte) ([]byte, error) {
	v := newViper()
	if err := readDoc(v, data); err != nil {
		return nil, err
	}
	maskSecrets(v)
	return dumpJSON(v)
}

// maskSecrets 就地把凭据路径替换为掩码
func maskSecrets(v *viper.Viper) {
	for _, p := range SecretPaths() {
		if v.GetString(p) != "" {
			v.Set(p, maskedValue)
		}
	}
}

func dumpJSON(v *viper.Viper) ([]byte, error) {
	b, err := json.MarshalIndent(v.AllSettings(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化配置失败: %w", err)
	}
	return b, nil
}

// RawFileJSON 原样读取 YAML/JSON 文件并转为 JSON 文档, 不合并任何默认值
// 供一次性 import 使用: 库里只存"用户显式配置过的项", 默认值继续由 newViper 单点提供,
// 否则默认值会被固化进文档, 日后改代码默认值压不住
func RawFileJSON(path string) ([]byte, error) {
	v := viper.New()
	if strings.HasSuffix(strings.ToLower(path), ".json") {
		v.SetConfigType("json")
	} else {
		v.SetConfigType("yaml")
	}
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	return dumpJSON(v)
}

// DocSet 按点路径改写文档中的一处, 返回新文档 (同样不注入默认值)
func DocSet(doc []byte, key, rawValue string) ([]byte, error) {
	section, _, found := strings.Cut(key, ".")
	if !found || !isKnownSection(section) {
		return nil, fmt.Errorf("未知配置段 %q, 写入后服务读不到; 可用段: %s",
			section, strings.Join(Sections(), ", "))
	}
	v := viper.New()
	v.SetConfigType("json")
	if err := readDoc(v, doc); err != nil {
		return nil, err
	}
	v.Set(key, parseScalar(rawValue))
	return dumpJSON(v)
}

// Sections 返回全部顶层配置段名, 取自 Config 的 mapstructure tag, 不另立清单
func Sections() []string {
	t := reflect.TypeOf(Config{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		if tag := t.Field(i).Tag.Get("mapstructure"); tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

func isKnownSection(section string) bool {
	for _, s := range Sections() {
		if strings.EqualFold(s, section) {
			return true
		}
	}
	return false
}

// DocGet 返回 key 的生效值 (代码默认值已合并)
func DocGet(doc []byte, key string) (any, error) {
	v := newViper()
	if err := readDoc(v, doc); err != nil {
		return nil, err
	}
	return v.Get(key), nil
}

// DocGetMasked 与 DocGet 同样读取点路径, 但凭据一律先掩码
// 必须整棵文档先脱敏再取值: 只判断叶子路径会漏掉父路径取值 (如 get mail 会带出 password)
func DocGetMasked(doc []byte, key string) (any, error) {
	v := newViper()
	if err := readDoc(v, doc); err != nil {
		return nil, err
	}
	maskSecrets(v)
	// 只能走 AllSettings: viper 把嵌套 Set 当成整段覆盖, Get("llm") 会只剩 api_key
	// 这一项, 表现为"整段配置凭空消失" (dump 一直用 AllSettings 所以没踩到)
	return lookupPath(v.AllSettings(), strings.Split(key, ".")), nil
}

// lookupPath 按点路径在已合并的配置 map 中取值, 任一层不存在或类型不符返回 nil
func lookupPath(root map[string]any, path []string) any {
	var cur any = root
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[p]; !ok {
			return nil
		}
	}
	return cur
}

// parseScalar 优先按 JSON 解析, 使数字/布尔/数组/对象字面量各归其位; 失败则按原始字符串处理
func parseScalar(raw string) any {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return raw
}

// IsSecretPath 判断配置路径是否存放凭据
func IsSecretPath(key string) bool {
	for _, p := range SecretPaths() {
		if strings.EqualFold(p, key) {
			return true
		}
	}
	return false
}

// maskedValue 凭据在 dump/日志中的替身
const maskedValue = "***"

// SecretPaths 返回存放凭据的配置路径, 任何 dump 与日志通道不得输出其明文
func SecretPaths() []string {
	return []string{"tushare.token", "llm.api_key", "mail.password", "server.api_token"}
}

// applyEnvOverrides 用环境变量覆盖凭据项 (非空才生效)
// 凭据已随配置存于 config_kv, 这里保留一条不落库即可顶换密钥的应急通道 (如库损坏/紧急轮换)
func applyEnvOverrides(cfg *Config) {
	if t := os.Getenv("TUSHARE_TOKEN"); t != "" {
		cfg.Tushare.Token = t
	}
	if k := os.Getenv("LLM_API_KEY"); k != "" {
		cfg.LLM.APIKey = k
	}
	if t := os.Getenv("JZ_API_TOKEN"); t != "" {
		cfg.Server.APIToken = t
	}
	if p := os.Getenv("JZ_MAIL_PASSWORD"); p != "" {
		cfg.Mail.Password = p
	}
}

// DefaultDBPath 返回默认数据库路径
// 配置整体存放在该库的 config_kv 表内, 因此库路径是唯一必须先于配置确定的外部输入 (由 -db flag 或 JZ_DB_PATH 覆盖)
func DefaultDBPath() string {
	return "data/jingzhe.db"
}
