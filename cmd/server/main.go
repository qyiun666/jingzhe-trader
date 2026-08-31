package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"jingzhe-trader/internal/api"
	"jingzhe-trader/internal/appcfg"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/mcp"
	"jingzhe-trader/internal/scheduler"
	"jingzhe-trader/pkg/logger"
)

func main() {
	port := flag.String("port", "11270", "监听端口")
	dbPath := flag.String("db", "", "数据库路径 (配置即存于此库, 默认取 "+appcfg.EnvDBPath+" 或 "+config.DefaultDBPath()+")")
	flag.Parse()

	// 先开库再读配置: 配置本体就存在 config_kv 里, 库是唯一前置输入
	// 装载失败直接退出, 禁止空配置兼容运行导致数据落错库
	resolvedDB := appcfg.ResolveDBPath(*dbPath)
	db, err := appcfg.Open(resolvedDB)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	cfg, err := appcfg.Load(db)
	if err != nil {
		db.Close()
		log.Fatalf("加载配置失败: %v", err)
	}

	// 初始化结构化日志 (与 backtest/dataloader 同一套配置驱动, 避免 zap 默认分支无 caller/无文件输出)
	logger.Init(cfg.Log.Level, cfg.Log.Format, cfg.Log.Output, cfg.Log.FilePath, cfg.Retention.LogDays)
	defer logger.Sync()

	// 优先使用配置文件中的端口
	serverPort := *port
	if cfg.Server.Port > 0 {
		serverPort = fmt.Sprintf("%d", cfg.Server.Port)
	}

	// 创建 API 服务 (数据库由本组合根持有, 服务只借用)
	svc, err := api.NewService(db, cfg)
	if err != nil {
		db.Close()
		log.Fatalf("初始化 API 服务失败: %v", err)
	}

	// 创建 MCP 服务器 (对外接口为 MCP over Streamable HTTP)
	mcpServer := mcp.NewServer(svc, cfg)
	handler := mcpServer.Handler()

	// 配置 HTTP 服务器 (默认仅本机监听, 由配置 server.host 控制)
	host := cfg.Server.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := host + ":" + serverPort

	// 包装 MCP handler: 提供 /health 给 systemd/cron, 并对 /mcp 复用 Bearer 鉴权
	handler = withHealthAndAuth(handler, svc, cfg.Server.APIToken)

	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 根 context: 所有后台 goroutine 由此派生, 信号触发统一退出
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 内置调度器 (数据更新/EOD信号/日报推送/盘中止损监控/数据清理)
	var wg sync.WaitGroup
	if cfg.Scheduler.Enabled {
		sched := scheduler.New(cfg, svc.DB(), svc)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sched.Start(rootCtx)
		}()
	} else {
		log.Println("调度器未启用 (scheduler.enabled=false)")
	}

	// 优雅关闭: 信号 → cancel根ctx → HTTP Shutdown → 等调度任务收尾 → db.Close
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("收到信号 %v, 停止服务...", sig)

		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("服务关闭异常: %v", err)
		}
	}()

	log.Printf("========================================")
	log.Printf("惊蛰交易系统 server 启动")
	log.Printf("地址: http://%s", addr)
	log.Printf("数据库: %s", resolvedDB)
	log.Printf("调度器: enabled=%v", cfg.Scheduler.Enabled)
	log.Printf("========================================")

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("服务异常: %v", err)
	}

	// 等调度器 goroutine 收尾后再关数据库, 保证 SQLite 不留脏 WAL
	wg.Wait()
	if err := db.Close(); err != nil {
		log.Printf("关闭数据库异常: %v", err)
	}
	log.Println("服务已停止")
}

// withHealthAndAuth wraps the MCP handler with a lightweight /health endpoint
// and reuses the existing Bearer token auth for the MCP endpoint.
func withHealthAndAuth(next http.Handler, svc *api.Service, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			writeJSON(w, http.StatusOK, svc.BuildHealthStatus())
			return
		}
		if token != "" && r.URL.Path == "/mcp" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+token {
				w.WriteHeader(http.StatusUnauthorized)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}
