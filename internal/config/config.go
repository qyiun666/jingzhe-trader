package config

import (
	"fmt"
	"os"
	"path/filepath"

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
	Feishu   FeishuConfig   `mapstructure:"feishu"`
	LLM      LLMConfig      `mapstructure:"llm"`
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
	Host string `mapstructure:"host"` // 监听地址
	Port int    `mapstructure:"port"` // 监听端口
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
	ShortPeriod int     `mapstructure:"short_period"`
	LongPeriod  int     `mapstructure:"long_period"`
	PositionPct float64 `mapstructure:"position_pct"`
}

// MACDConfig MACD策略配置
type MACDConfig struct {
	Fast        int     `mapstructure:"fast"`
	Slow        int     `mapstructure:"slow"`
	Signal      int     `mapstructure:"signal"`
	PositionPct float64 `mapstructure:"position_pct"`
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
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("feishu.push_daily", false)
	v.SetDefault("feishu.push_time", "15:30")
	v.SetDefault("llm.enabled", false)
	v.SetDefault("llm.base_url", "https://api.deepseek.com/v1")
	v.SetDefault("llm.model", "deepseek-chat")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}

	// 确保数据目录存在
	dbDir := filepath.Dir(cfg.Database.Path)
	if dbDir != "" && dbDir != "." {
		os.MkdirAll(dbDir, 0755)
	}

	return &cfg, nil
}

// DefaultConfigPath 返回默认配置文件路径
func DefaultConfigPath() string {
	return "config/config.yaml"
}
