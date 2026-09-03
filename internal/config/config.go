package config

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

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
func effectiveValue(spec KeySpec, row store.ConfigRow, dbHas bool) string {
	v := spec.Default
	if dbHas {
		v = row.Value
	}
	if env := os.Getenv(envName(spec.Key)); env != "" {
		v = env // 应急覆盖通道：非空才顶换库内值（§6.3）
	}
	return v
}

// Load 从 config_kv 读取配置，叠加默认值与环境变量，并拒绝危险零值/缺失必配项（启动自检 D6）。
func Load(ctx context.Context, s *store.Store) (*Config, error) {
	rows, err := s.ConfigRepo().GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	db := make(map[string]store.ConfigRow, len(rows))
	for _, r := range rows {
		db[r.Key] = r
	}
	vals := make(map[string]ConfigValue, len(KeySpecs))
	for _, spec := range KeySpecs {
		row, ok := db[spec.Key]
		v := effectiveValue(spec, row, ok)
		vals[spec.Key] = ConfigValue{Key: spec.Key, Type: spec.Type, Value: v, IsSecret: spec.Secret}
	}
	cfg := &Config{Values: vals}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate 校验：必配项为空 → 列出缺失；拒绝零值键为 0 → 列出零值（D6）。
func (c *Config) Validate() error {
	var missing, zeros []string
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
	}
	if len(missing) == 0 && len(zeros) == 0 {
		return nil
	}
	return &ConfigError{Missing: missing, ZeroValues: zeros}
}

// ConfigError 配置自检失败（含缺失项与零值项清单）。
type ConfigError struct {
	Missing    []string
	ZeroValues []string
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
	db := make(map[string]store.ConfigRow, len(rows))
	for _, r := range rows {
		db[r.Key] = r
	}
	entries := make([]Entry, 0, len(KeySpecs))
	for _, key := range SortedKeys() {
		spec := specIndex[key]
		row, ok := db[key]
		v := effectiveValue(spec, row, ok)
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
	db := make(map[string]store.ConfigRow)
	if rows, err := s.ConfigRepo().GetAll(ctx); err == nil {
		for _, r := range rows {
			db[r.Key] = r
		}
	}
	row, ok := db[key]
	v := effectiveValue(spec, row, ok)
	return Entry{Key: key, Value: v, Type: spec.Type, IsSecret: spec.Secret}, nil
}

// DisplayValue 按掩码规则返回展示值：凭据且未显式 --show-secrets 时返回掩码。
func DisplayValue(e Entry, showSecrets bool) string {
	if e.IsSecret && !showSecrets {
		return Mask(e.Value)
	}
	return e.Value
}
