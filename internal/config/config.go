package config

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// ConfigValue 单个配置项的类型化视图。
type ConfigValue struct {
	Key      string
	Type     KeyType
	Value    string
	IsSecret bool
}

// Config 全量配置（内存视图），由 Load 构建。
type Config struct {
	Values map[string]ConfigValue
}

// Entry dump/get 的输出项。
type Entry struct {
	Key      string
	Value    string
	Type     KeyType
	IsSecret bool
}

// effectiveValue 计算生效值：默认 < 库值 < 环境变量（非空才顶换）。
// write-once 键例外：库里已有非零值时环境变量不得顶换。
func effectiveValue(spec KeySpec, dbValue string, dbHas bool) string {
	v := spec.Default
	if dbHas {
		v = dbValue
	}
	env := os.Getenv(envName(spec.Key))
	if env == "" {
		return v
	}
	// 本金这类 write-once 基准一旦被一份 .env 悄悄改写，季度收益与回撤全错
	// （历史事故："同步把本金刷小"）。此时以库值为准并显式告警，不静默丢弃。
	if spec.WriteOnce && v != "" && v != "0" {
		observability.S().Warnw("忽略 write-once 配置键的环境变量覆盖，以库内值为准",
			"key", spec.Key, "db_value", v)
		return v
	}
	return env
}

// Load 从 config_kv 读取配置，叠加默认值与环境变量，并拒绝危险零值/缺失必配项（启动自检 D6）。
func Load(ctx context.Context, s *store.Store) (*Config, error) {
	rows, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	vals := make(map[string]ConfigValue, len(KeySpecs))
	for _, spec := range KeySpecs {
		dbValue, ok := rows[spec.Key]
		v := effectiveValue(spec, dbValue, ok)
		vals[spec.Key] = ConfigValue{Key: spec.Key, Type: spec.Type, Value: v, IsSecret: spec.Secret}
	}
	cfg := &Config{Values: vals}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 校验三类：必配项为空、拒绝零值键为 0、数值/布尔键解析不了（启动自检 D6）。
//
// 第三类必须有：GetInt/GetFloat/GetBool 解析失败一律返回零值。
// 一个写坏的数值键（手工 SQL 改的、或老版本写进去的）在业务层静默变成 0，
// 比键干脆缺失更隐蔽 —— 门禁阈值类配置尤其如此（阈值为 0 等于门禁不生效）。
func (c *Config) Validate() error {
	var missing, zeros, malformed []string
	for _, spec := range KeySpecs {
		v := c.val(spec.Key)
		if spec.Required && strings.TrimSpace(v) == "" {
			missing = append(missing, spec.Key)
			continue
		}
		if spec.RefuseZero {
			switch spec.Type {
			case TypeFloat:
				if f, err := strconv.ParseFloat(v, 64); err == nil && f == 0 {
					zeros = append(zeros, spec.Key)
				}
			case TypeInt:
				if i, err := strconv.Atoi(v); err == nil && i == 0 {
					zeros = append(zeros, spec.Key)
				}
			}
		}
		if v = strings.TrimSpace(v); v != "" {
			if err := coerce(spec, v); err != nil {
				malformed = append(malformed, fmt.Sprintf("%s=%s（%s）", spec.Key, v, err))
			}
		}
	}
	if len(missing) == 0 && len(zeros) == 0 && len(malformed) == 0 {
		return nil
	}
	return &ConfigError{Missing: missing, ZeroValues: zeros, Malformed: malformed}
}

// ConfigError 配置自检失败（含缺失项、零值项与类型不符项清单）。
type ConfigError struct {
	Missing    []string
	ZeroValues []string
	Malformed  []string
}

func (e *ConfigError) Error() string {
	var b strings.Builder
	b.WriteString("配置自检未通过：")
	for _, k := range e.Missing {
		b.WriteString(fmt.Sprintf("\n  - 缺少必配项: %s（必填，不能为空）", k))
	}
	for _, k := range e.ZeroValues {
		b.WriteString(fmt.Sprintf("\n  - %s 为零值（危险配置，必须显式设置非零值）", k))
	}
	for _, k := range e.Malformed {
		b.WriteString(fmt.Sprintf("\n  - 配置值与声明类型不符: %s（读取时会静默变成零值）", k))
	}
	return b.String()
}

// ===================== 类型化访问器 =====================

func (c *Config) val(k string) string {
	if v, ok := c.Values[k]; ok {
		return v.Value
	}
	return ""
}

// GetString 读取字符串值。
func (c *Config) GetString(k string) string { return c.val(k) }

// GetInt 读取整数值（解析失败返回 0）。
func (c *Config) GetInt(k string) int {
	i, _ := strconv.Atoi(c.val(k))
	return i
}

// GetFloat 读取浮点值（解析失败返回 0）。
func (c *Config) GetFloat(k string) float64 {
	f, _ := strconv.ParseFloat(c.val(k), 64)
	return f
}

// GetBool 读取布尔值（解析失败返回 false）。
func (c *Config) GetBool(k string) bool {
	b, _ := strconv.ParseBool(c.val(k))
	return b
}

// ===================== dump / get（不校验，供 CLI 查看）=====================

// Dump 返回全部配置键的生效值（叠加默认值与环境变量），不触发校验。
func Dump(ctx context.Context, s *store.Store) ([]Entry, error) {
	rows, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	entries := make([]Entry, 0, len(KeySpecs))
	for _, key := range SortedKeys() {
		spec := specIndex[key]
		stored, ok := rows[key]
		v := effectiveValue(spec, stored, ok)
		entries = append(entries, Entry{Key: key, Value: v, Type: spec.Type, IsSecret: spec.Secret})
	}
	return entries, nil
}

// Get 返回单个配置键的生效值（未知键返回 error）。
func Get(ctx context.Context, s *store.Store, key string) (Entry, error) {
	spec, ok := FindSpec(key)
	if !ok {
		return Entry{}, fmt.Errorf("未知配置键: %s", key)
	}
	rows, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		return Entry{}, fmt.Errorf("读取配置失败: %w", err)
	}
	stored, ok := rows[key]
	v := effectiveValue(spec, stored, ok)
	return Entry{Key: key, Value: v, Type: spec.Type, IsSecret: spec.Secret}, nil
}

// DisplayValue 按掩码规则返回展示值：凭据且未显式 --show-secrets 时返回掩码。
func DisplayValue(e Entry, showSecrets bool) string {
	if e.IsSecret && !showSecrets {
		return Mask(e.Value)
	}
	return e.Value
}
