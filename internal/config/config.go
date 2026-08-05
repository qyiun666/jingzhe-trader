package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Config 全局配置
type Config struct {
	Tushare  TushareConfig  `mapstructure:"tushare"`
	Database DatabaseConfig `mapstructure:"database"`
	Cost     CostConfig     `mapstructure:"cost"`
	Backtest BacktestConfig `mapstructure:"backtest"`
	Risk     RiskConfig     `mapstructure:"risk"`
	Log      LogConfig      `mapstructure:"log"`
	Broker   BrokerConfig   `mapstructure:"broker"`
	Strategy StrategyConfig `mapstructure:"strategy"`
	Universe UniverseConfig `mapstructure:"universe"`
	Server   ServerConfig   `mapstructure:"server"`
	Feishu     FeishuConfig     `mapstructure:"feishu"`
	LLM        LLMConfig        `mapstructure:"llm"`
	Dataloader DataloaderConfig `mapstructure:"dataloader"`
	Scheduler  SchedulerConfig  `mapstructure:"scheduler"`
	Trading    TradingConfig    `mapstructure:"trading"`
	Retention  RetentionConfig  `mapstructure:"retention"`
	Screener  ScreenerConfig  `mapstructure:"screener"`
}

// SchedulerConfig 内置调度器配置
type SchedulerConfig struct {
	Enabled        bool           `mapstructure:"enabled"`          // 是否启用调度器
	DataUpdateTime string         `mapstructure:"data_update_time"` // 数据更新时间 HH:MM
	ScreenerTime   string         `mapstructure:"screener_time"`    // 自动选股时间 HH:MM
	SignalTime     string         `mapstructure:"signal_time"`      // EOD信号生成时间 HH:MM
	ReportTime     string         `mapstructure:"report_time"`      // 日报生成时间 HH:MM
	Intraday       IntradayConfig `mapstructure:"intraday"`         // 盘中监控
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
	PlanDays     int    `mapstructure:"plan_days"`     // 交易计划保留天数
	BacktestRuns int    `mapstructure:"backtest_runs"` // 保留最近N个回测run
	LogDays      int    `mapstructure:"log_days"`      // 日志文件保留天数
	ReportFiles  int    `mapstructure:"report_files"`  // 保留最近N个报告文件
	CleanupTime  string `mapstructure:"cleanup_time"`  // 每日清理时间 HH:MM
}

// ScreenerConfig 自动选股器配置
type ScreenerConfig struct {
	Enabled         bool     `mapstructure:"enabled"`          // 是否启用选股器
	MaxCandidates   int      `mapstructure:"max_candidates"`   // 最大候选股票数
	ExcludeCodes    []string `mapstructure:"exclude_codes"`    // 排除的股票代码 (配置池已有)
	ExcludeST       bool     `mapstructure:"exclude_st"`       // 排除ST股
	MinListDays     int      `mapstructure:"min_list_days"`    // 最小上市天数 (排除新股)
	MinPrice        float64  `mapstructure:"min_price"`        // 最低股价
	MaxPrice        float64  `mapstructure:"max_price"`        // 最高股价
	MinTurnoverRate float64  `mapstructure:"min_turnover_rate"` // 最低换手率 %
	MaxPE           float64  `mapstructure:"max_pe"`           // 最大PE_TTM
	MaxPB           float64  `mapstructure:"max_pb"`           // 最大PB
	MinCircMV       float64  `mapstructure:"min_circ_mv"`      // 最小流通市值 (万元)
	MaxCircMV       float64  `mapstructure:"max_circ_mv"`      // 最大流通市值 (万元)
}

// DataloaderConfig 数据加载器配置
type DataloaderConfig struct {
	FilterMode  bool     `mapstructure:"filter_mode"`  // 筛选模式: 只拉取股票池+持仓+关注列表的股票
	Watchlist   []string `mapstructure:"watchlist"`    // 额外关注的股票代码列表
	EnableLimit bool     `mapstructure:"enable_limit"`   // 是否同步涨跌停价
	EnableBasic bool     `mapstructure:"enable_basic"`   // 是否同步每日基本面
	EnableFund  bool     `mapstructure:"enable_fund"`    // 是否同步ETF/基金日线
	EnableCleanup bool   `mapstructure:"enable_cleanup"` // 是否允许清理非关注股票数据(危险操作, 默认关闭)
}

// LLMConfig LLM 配置
// 用于新闻深度分析和选股辅助，不直接做交易决策
// 默认关闭，不影响系统核心功能
type LLMConfig struct {
	Enabled bool   `mapstructure:"enabled"`  // 是否启用 LLM
	APIKey  string `mapstructure:"api_key"`  // API Key
	BaseURL string `mapstructure:"base_url"` // API 地址，默认 DeepSeek
	Model   string `mapstructure:"model"`    // 模型名称，默认 deepseek-chat
}

// ServerConfig HTTP 服务配置
type ServerConfig struct {
	Host           string   `mapstructure:"host"`            // 监听地址
	Port           int      `mapstructure:"port"`            // 监听端口
	APIToken       string   `mapstructure:"api_token"`       // API鉴权token, 非空时启用Bearer校验
	AllowedOrigins []string `mapstructure:"allowed_origins"` // CORS允许的来源列表
}

// FeishuConfig 飞书通知配置
type FeishuConfig struct {
	WebhookURL string `mapstructure:"webhook_url"` // 飞书机器人 webhook URL
	PushDaily  bool   `mapstructure:"push_daily"`  // 是否每天自动推送
	PushTime   string `mapstructure:"push_time"`   // 推送时间 HH:MM
}

type TushareConfig struct {
	Token         string `mapstructure:"token"`
	BaseURL       string `mapstructure:"base_url"`
	RateLimit     int    `mapstructure:"rate_limit"`
	MaxRetries    int    `mapstructure:"max_retries"`
	RetryInterval int    `mapstructure:"retry_interval"`
}

type DatabaseConfig struct {
	Path string `mapstructure:"path"`
}

type CostConfig struct {
	CommissionRate   float64 `mapstructure:"commission_rate"`
	MinCommission    float64 `mapstructure:"min_commission"`
	StampTaxRate     float64 `mapstructure:"stamp_tax_rate"`
	TransferFeeRate  float64 `mapstructure:"transfer_fee_rate"`
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
	Type string       `mapstructure:"type"` // paper / qmt
	QMT  QMTConfig    `mapstructure:"qmt"`
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
	StopLossPct     float64 `mapstructure:"stop_loss_pct"`
	TakeProfitPct  float64 `mapstructure:"take_profit_pct"`
}

// UniverseConfig 股票池配置
type UniverseConfig struct {
	Bluechip string `mapstructure:"bluechip"`
	Tech     string `mapstructure:"tech"`
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
	case "macd":
		params["fast"] = float64(c.Strategy.MACD.Fast)
		params["slow"] = float64(c.Strategy.MACD.Slow)
		params["signal"] = float64(c.Strategy.MACD.Signal)
		params["position_pct"] = c.Strategy.MACD.PositionPct
		params["enable_adaptive"] = c.Strategy.MACD.EnableAdaptive
	case "multi_factor":
		params["position_pct"] = c.Strategy.MultiFactor.PositionPct
	default:
		params["position_pct"] = 0.15
	}
	return params
}

// Load 加载配置文件
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.AutomaticEnv()

	// 设置默认值
	v.SetDefault("tushare.base_url", "http://api.tushare.pro")
	v.SetDefault("tushare.rate_limit", 450)
	v.SetDefault("tushare.max_retries", 3)
	v.SetDefault("tushare.retry_interval", 2)
	v.SetDefault("database.path", "data/jingzhe.db")
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
	v.SetDefault("feishu.push_daily", false)
	v.SetDefault("feishu.push_time", "15:30")
	v.SetDefault("llm.enabled", false)
	v.SetDefault("llm.base_url", "https://api.deepseek.com/v1")
	v.SetDefault("llm.model", "deepseek-chat")
	v.SetDefault("dataloader.filter_mode", false)
	v.SetDefault("dataloader.enable_limit", true)
	v.SetDefault("dataloader.enable_basic", true)
	v.SetDefault("dataloader.enable_fund", true)
	v.SetDefault("dataloader.enable_cleanup", false)
	v.SetDefault("scheduler.enabled", true)
	v.SetDefault("scheduler.data_update_time", "15:10")
	v.SetDefault("scheduler.screener_time", "15:15")
	v.SetDefault("scheduler.signal_time", "15:30")
	v.SetDefault("scheduler.report_time", "15:45")
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

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}

	// 敏感项环境变量优先, 避免密钥写入配置文件
	applyEnvOverrides(&cfg)

	// 确保数据目录存在
	dbDir := filepath.Dir(cfg.Database.Path)
	if dbDir != "" && dbDir != "." {
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	return &cfg, nil
}

// applyEnvOverrides 用环境变量覆盖敏感配置项
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
	if w := os.Getenv("FEISHU_WEBHOOK"); w != "" {
		cfg.Feishu.WebhookURL = w
	}
}

// DefaultConfigPath 返回默认配置文件路径
func DefaultConfigPath() string {
	return "config/config.yaml"
}
