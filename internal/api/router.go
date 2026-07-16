package api

import (
	"log"
	"net/http"

)

// corsMiddleware CORS 中间件 (支持所有来源, 适用于 NAS 局域网)
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 处理 OPTIONS 预检请求
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// NewRouter 创建路由
func NewRouter(svc *Service) http.Handler {
	mux := http.NewServeMux()

	// 仪表盘首页
	mux.HandleFunc("/", svc.HandleDashboard)

	// 核心接口
	mux.HandleFunc("/api/daily", svc.HandleDaily)       // GET 每日操盘报告(汇总)
	mux.HandleFunc("/api/positions", svc.HandlePositions) // GET 持仓诊断
	mux.HandleFunc("/api/rebalance", svc.HandleRebalance) // GET 调仓建议
	mux.HandleFunc("/api/news", svc.HandleNews)          // GET 新闻舆情
	mux.HandleFunc("/api/news/llm", svc.HandleLLMNews)   // GET LLM深度新闻分析
	mux.HandleFunc("/api/strategy", svc.HandleStrategy)  // GET 策略建议
	mux.HandleFunc("/api/market", svc.HandleMarket)      // GET 市场概况

	// 仪表盘专用接口
	mux.HandleFunc("/api/kline", svc.HandleKline)         // GET K线数据
	mux.HandleFunc("/api/snapshots", svc.HandleSnapshots)  // GET 账户快照历史

	// 基础接口
	mux.HandleFunc("/api/health", svc.HandleHealth) // GET 健康检查

	// 持仓管理
	mux.HandleFunc("/api/portfolio", svc.HandleGetPortfolio)       // GET 获取持仓
	mux.HandleFunc("/api/portfolio/sync", svc.HandleSyncPortfolio) // POST 同步持仓

	// 交易反馈
	mux.HandleFunc("/api/trade/confirm", svc.HandleTradeConfirm) // POST 交易确认

	// 动态策略
	mux.HandleFunc("/api/strategy/status", svc.HandleStrategyStatus) // GET 策略状态

	// 系统维护
	mux.HandleFunc("/api/system/status", svc.HandleSystemStatus)     // GET 系统状态
	mux.HandleFunc("/api/system/update-data", svc.HandleUpdateData) // POST 触发数据更新

	// 打印所有路由
	routes := []struct {
		path   string
		method string
		desc   string
	}{
		{"/", "GET", "仪表盘首页"},
		{"/api/health", "GET", "健康检查"},
		{"/api/daily", "GET", "每日操盘报告(汇总)"},
		{"/api/positions", "GET", "持仓诊断"},
		{"/api/rebalance", "GET", "调仓建议"},
		{"/api/news", "GET", "新闻舆情"},
		{"/api/news/llm", "GET", "LLM深度新闻分析"},
		{"/api/strategy", "GET", "策略建议"},
		{"/api/market", "GET", "市场概况"},
		{"/api/kline", "GET", "K线数据"},
		{"/api/snapshots", "GET", "账户快照历史"},
		{"/api/portfolio", "GET", "获取持仓列表"},
		{"/api/portfolio/sync", "POST", "同步持仓"},
		{"/api/trade/confirm", "POST", "交易反馈确认"},
		{"/api/strategy/status", "GET", "动态策略状态"},
		{"/api/system/status", "GET", "系统状态"},
		{"/api/system/update-data", "POST", "手动数据更新"},
	}

	for _, route := range routes {
		log.Printf("  [路由] %s %s - %s", route.method, route.path, route.desc)
	}

	// 包装 CORS 中间件
	return corsMiddleware(mux)
}
