package main

import (
	"context"
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
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/scheduler"
)

func main() {
	port := flag.String("port", "11270", "监听端口")
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	flag.Parse()

	// 加载配置 (失败直接退出, 禁止空配置兼容运行导致数据落错库)
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}

	// 优先使用配置文件中的端口
	serverPort := *port
	if cfg.Server.Port > 0 {
		serverPort = fmt.Sprintf("%d", cfg.Server.Port)
	}

	// 创建 API 服务
	svc, err := api.NewService(cfg)
	if err != nil {
		log.Fatalf("初始化 API 服务失败: %v", err)
	}

	// 创建路由
	handler := api.NewRouter(svc)

	// 配置 HTTP 服务器 (默认仅本机监听, 由配置 server.host 控制)
	host := cfg.Server.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := host + ":" + serverPort
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
	log.Printf("配置: %s", *configPath)
	log.Printf("调度器: enabled=%v", cfg.Scheduler.Enabled)
	log.Printf("========================================")

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("服务异常: %v", err)
	}

	// 等调度器 goroutine 收尾后再关数据库, 保证 SQLite 不留脏 WAL
	wg.Wait()
	if err := svc.Close(); err != nil {
		log.Printf("关闭数据库异常: %v", err)
	}
	log.Println("服务已停止")
}
