package main

import (
	"context"
	"fmt"
	"os"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/store"
)

// runConfig 处理 `jingzhe config <dump|get KEY|set KEY VALUE>`。
// 凭据键默认掩码，需显式 --show-secrets 才显示明文。
func runConfig(ctx context.Context, st *store.Store, args []string) {
	showSecrets := false
	pos := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--show-secrets" {
			showSecrets = true
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "用法: jingzhe config <dump|get KEY|set KEY VALUE> [--show-secrets]")
		os.Exit(2)
	}

	switch pos[0] {
	case "dump":
		entries, err := config.Dump(ctx, st)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dump 失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%-32s %-8s %s\n", "KEY", "TYPE", "VALUE")
		for _, e := range entries {
			fmt.Printf("%-32s %-8s %s\n", e.Key, e.Type, config.DisplayValue(e, showSecrets))
		}
	case "get":
		if len(pos) < 2 {
			fmt.Fprintln(os.Stderr, "用法: jingzhe config get KEY [--show-secrets]")
			os.Exit(2)
		}
		e, err := config.Get(ctx, st, pos[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Println(config.DisplayValue(e, showSecrets))
	case "set":
		if len(pos) < 3 {
			fmt.Fprintln(os.Stderr, "用法: jingzhe config set KEY VALUE")
			os.Exit(2)
		}
		if err := setConfig(ctx, st, pos[1], pos[2]); err != nil {
			fmt.Fprintf(os.Stderr, "set 失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已写入配置 %s\n", pos[1])
	default:
		fmt.Fprintf(os.Stderr, "未知 config 子命令: %s\n", pos[0])
		os.Exit(2)
	}
}

// setConfig 写入配置：含 write-once 保护与类型校验（由 config.Repo.Set 完成）。
func setConfig(ctx context.Context, st *store.Store, key, value string) error {
	spec, ok := config.FindSpec(key)
	if !ok {
		return fmt.Errorf("未知配置键: %s（可用 jingzhe config dump 查看全部）", key)
	}
	if spec.WriteOnce {
		raw, err := config.NewRepo(st).RawAll(ctx)
		if err != nil {
			return fmt.Errorf("读取现有配置失败: %w", err)
		}
		if _, exists := raw[key]; exists {
			return fmt.Errorf("配置键 %s 为 write-once，已写入不可覆盖（如需变更请走人工复核流程）", key)
		}
	}
	return config.NewRepo(st).Set(ctx, key, value)
}
