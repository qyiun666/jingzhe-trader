package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jingzhe-trader/internal/api"
	"jingzhe-trader/internal/config"
)

func main() {
	port := flag.String("port", "11270", "监听端口")
	configPath := flag.String("config", "config/config.yaml", "配置文件路径")
	flag.Parse()

	// 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Printf("加载配置失败: %v, 使用默认配置", err)
		cfg = &config.Config{}
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
	defer svc.Close()

	// 创建路由
	handler := api.NewRouter(svc)

	// 配置 HTTP 服务器
	addr := "0.0.0.0:" + serverPort
	srv := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 优雅关闭
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		sig := <-sigCh
		log.Printf("收到信号 %v, 停止服务...", sig)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("服务关闭异常: %v", err)
		}
	}()

	log.Printf("========================================")
	log.Printf("chaogu API 服务启动")
	log.Printf("地址: http://%s", addr)
	log.Printf("配置: %s", *configPath)
	log.Printf("========================================")

	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("服务异常: %v", err)
	}

	log.Println("服务已停止")
}