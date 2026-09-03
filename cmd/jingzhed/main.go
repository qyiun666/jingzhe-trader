// 惊蛰常驻服务 jingzhed：对外暴露 MCP 接口（默认 :8080），供外部 AI agent 读取状态、
// 回执成交、触发当日流程。NAS 上以 systemd/launchd 守护，7×24 运行。
//
// 用法:
//   jingzhed -db data/jingzhe.db [-addr :8080]
//
// API 令牌优先级：环境变量 JZ_SERVER_API_TOKEN > config server.api_token。
// 令牌为空时服务拒绝启动（验收 §10.6-7）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/mcp"
	"jingzhe-trader/internal/store"
)

func main() {
	dbPath := flag.String("db", "data/jingzhe.db", "SQLite 数据库路径")
	addr := flag.String("addr", ":8080", "MCP 服务监听地址")
	flag.Parse()

	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	cfg, err := config.Load(context.Background(), st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	apiToken := os.Getenv("JZ_SERVER_API_TOKEN")
	if apiToken == "" {
		apiToken = cfg.GetString("server.api_token")
	}

	srv, err := mcp.New(st, cfg, apiToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP 服务启动失败: %v\n", err)
		os.Exit(1)
	}
	if err := srv.Start(*addr); err != nil {
		fmt.Fprintf(os.Stderr, "MCP 服务异常退出: %v\n", err)
		os.Exit(1)
	}
}
