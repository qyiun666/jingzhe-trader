package api

import (
	"log"
	"net/http"
	"runtime/debug"
	"strings"
)

// recoverMiddleware 捕获 handler panic, 防止单个请求异常导致进程退出
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[panic] %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware CORS 中间件, 仅允许配置中的来源
func corsMiddleware(allowedOrigins []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && originAllowed(origin, allowedOrigins) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		// 处理 OPTIONS 预检请求
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed 判断来源是否在白名单中 (忽略端口)
func originAllowed(origin string, allowed []string) bool {
	for _, a := range allowed {
		if origin == a || strings.HasPrefix(origin, a+":") {
			return true
		}
	}
	return false
}

// authMiddleware API 鉴权中间件
// api_token 配置非空时, 所有写请求(非GET/OPTIONS)必须携带 Authorization: Bearer <token>
func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Method != http.MethodGet && r.Method != http.MethodOptions {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+token {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
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
	mux.HandleFunc("/api/daily", svc.HandleDaily)         // GET 每日操盘报告(汇总)
	mux.HandleFunc("/api/positions", svc.HandlePositions) // GET 持仓诊断
	mux.HandleFunc("/api/rebalance", svc.HandleRebalance) // GET 调仓建议
	mux.HandleFunc("/api/news", svc.HandleNews)           // GET 新闻舆情
	mux.HandleFunc("/api/news/llm", svc.HandleLLMNews)    // GET LLM深度新闻分析
	mux.HandleFunc("/api/strategy", svc.HandleStrategy)   // GET 策略建议
	mux.HandleFunc("/api/market", svc.HandleMarket)       // GET 市场概况

	// 仪表盘专用接口
	mux.HandleFunc("/api/kline", svc.HandleKline)         // GET K线数据
	mux.HandleFunc("/api/snapshots", svc.HandleSnapshots) // GET 账户快照历史

	// 基础接口
	mux.HandleFunc("/api/health", svc.HandleHealth) // GET 健康检查

	// 持仓管理
	mux.HandleFunc("/api/portfolio", svc.HandleGetPortfolio)       // GET 获取持仓
	mux.HandleFunc("/api/portfolio/sync", svc.HandleSyncPortfolio) // POST 同步持仓

	// 交易反馈
	mux.HandleFunc("/api/trade/confirm", svc.HandleTradeConfirm) // POST 交易确认

	// Agent 专用接口
	mux.HandleFunc("/api/agent/brief", svc.HandleAgentBrief)   // GET Agent全量上下文
	mux.HandleFunc("/api/agent/changes", svc.HandleAgentChanges) // GET 决策变更检测
	mux.HandleFunc("/api/plan", svc.HandlePlanList)          // GET 交易计划列表
	mux.HandleFunc("/api/plan/confirm", svc.HandlePlanConfirm) // POST 确认交易计划
	mux.HandleFunc("/api/reconcile", svc.HandleReconcile)    // GET 持仓对账

	// 动态策略
	mux.HandleFunc("/api/strategy/status", svc.HandleStrategyStatus) // GET 策略状态

	// 系统维护
	mux.HandleFunc("/api/system/status", svc.HandleSystemStatus)    // GET 系统状态
	mux.HandleFunc("/api/system/update-data", svc.HandleUpdateData) // POST 触发数据更新

	// 中间件链: recover → cors → auth → mux
	var handler http.Handler = mux
	handler = authMiddleware(svc.cfg.Server.APIToken, handler)
	handler = corsMiddleware(svc.cfg.Server.AllowedOrigins, handler)
	handler = recoverMiddleware(handler)
	return handler
}
