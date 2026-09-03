// 惊蛰运维 CLI jingzhectl：查看/修改 config_kv 中的配置（凭据默认掩码）。
//
// 用法:
//   jingzhectl -db data/jingzhe.db config dump [--show-secrets]
//   jingzhectl -db data/jingzhe.db config get <KEY> [--show-secrets]
//   jingzhectl -db data/jingzhe.db config set <KEY> <VALUE>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"jingzhe-trader/internal/store"
)

func main() {
	dbPath := flag.String("db", "data/jingzhe.db", "SQLite 数据库路径")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: jingzhectl <子命令> [参数]")
		fmt.Fprintln(os.Stderr, "子命令:")
		fmt.Fprintln(os.Stderr, "  config <dump|get KEY|set KEY VALUE>   查看/修改 config_kv（凭据默认掩码）")
		fmt.Fprintln(os.Stderr, "  run task <calendar|daily|fina|freshness|screener|signal>  运行任务")
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	switch args[0] {
	case "config":
		runConfig(context.Background(), st, args[1:])
	case "run":
		runRun(context.Background(), st, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", args[0])
		os.Exit(2)
	}
}
