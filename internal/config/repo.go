package config

import (
	"context"
	"fmt"
	"strconv"

	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// Repo config_kv 读写仓储（封装 store.ConfigRepo + 默认值目录 + 环境变量应急覆盖由上层处理）。
type Repo struct {
	cr *store.ConfigRepo
}

// NewRepo 构建配置仓储。
func NewRepo(s *store.Store) *Repo {
	return &Repo{cr: s.ConfigRepo()}
}

// RawAll 读取库里实际存有值的键（含未落默认值的键不在内）。
func (r *Repo) RawAll(ctx context.Context) (map[string]string, error) {
	m, err := r.cr.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %w", err)
	}
	return m, nil
}

// coerce 校验值是否符合类型（set 时前置校验）。
func coerce(spec KeySpec, value string) error {
	switch spec.Type {
	case TypeInt:
		if _, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("需为整数: %w", err)
		}
	case TypeFloat:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return fmt.Errorf("需为浮点数: %w", err)
		}
	case TypeBool:
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("需为布尔(true/false): %w", err)
		}
	}
	return nil
}

// Set 写入配置（未知键/类型错误返回 error）。
//
// 键的类型与凭据标记只活在键目录里，库里不存镜像；改动留痕写日志。
func (r *Repo) Set(ctx context.Context, key, value string) error {
	spec, ok := FindSpec(key)
	if !ok {
		return fmt.Errorf("未知配置键: %s（可用 jingzhe config dump 查看全部）", key)
	}
	if err := coerce(spec, value); err != nil {
		return fmt.Errorf("配置值类型错误 %s: %w", key, err)
	}
	if err := r.cr.Set(ctx, key, value); err != nil {
		return fmt.Errorf("写入配置 %s 失败: %w", key, err)
	}
	if spec.Secret {
		// 凭据只记"改了哪个键"，值不进日志（G-02）。
		observability.S().Infow("配置已更新", "key", key, "secret", true)
		return nil
	}
	observability.S().Infow("配置已更新", "key", key, "value", value)
	return nil
}

// SeedDefaults 补齐缺失的默认键（仅当键不存在时写入，不覆盖已有值）。
// 注意：Load/Dump 已在内存层覆盖默认值，本方法用于显式初始化命令，避免在只读路径上改写库。
func (r *Repo) SeedDefaults(ctx context.Context) error {
	have, err := r.cr.GetAll(ctx)
	if err != nil {
		return fmt.Errorf("读取现有配置失败: %w", err)
	}
	for _, spec := range KeySpecs {
		if _, ok := have[spec.Key]; ok {
			continue // 已存在，保留
		}
		if err := r.cr.Set(ctx, spec.Key, spec.Default); err != nil {
			return fmt.Errorf("补齐默认配置 %s 失败: %w", spec.Key, err)
		}
	}
	return nil
}
