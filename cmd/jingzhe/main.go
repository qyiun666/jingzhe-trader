// 惊蛰 jingzhe：单一二进制。
//
// 它同时是常驻服务和运维 CLI——一个进程里跑「定时任务调度 + MCP 对外接口」，
// NAS 上以后台方式启动一次即可；进程若退出，由外部 Agent 通过 MCP 探测发现。
//
// 用法:
//
//	jingzhe [-db data/jingzhe.db]                   启动常驻服务（默认子命令 serve）
//	jingzhe serve [-db ...] [-addr :8080]           启动常驻服务：调度器 + MCP 接口
//	jingzhe jobs  [-db ...] [-date 20260901]        演练当日时间线（dry-run，不真正执行）
//	jingzhe config <dump|get KEY|set KEY VALUE>     查看/修改 config_kv（凭据默认掩码）
//	jingzhe init   [-capital 元] [-cash 元] [-hold 代码:股数:成本,…]
//	                                             写入账户基线（本金/持仓/可用资金）
//	jingzhe run task <任务名> [--date YYYYMMDD]
//	                                             调度器任务：morning_plan/intraday_scan/evening_pipeline/mail_pending/daily_report
//	                                             数据面任务：calendar/daily/freshness/screen
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/mcp"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/scheduler"
	"jingzhe-trader/internal/store"
)

func main() {
	dbPath := flag.String("db", "data/jingzhe.db", "SQLite 数据库路径")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		args = []string{"serve"} // 无子命令即常驻服务
	}

	ctx := context.Background()
	st, err := store.Open(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}

	code := 0
	switch args[0] {
	case "config":
		runConfig(ctx, st, args[1:])
	case "init":
		runInit(ctx, st, args[1:])
	case "run":
		runRun(ctx, st, args[1:])
	case "jobs":
		runJobs(ctx, st, args[1:])
	case "serve":
		code = runServe(ctx, st, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: %s\n", args[0])
		usage()
		os.Exit(2)
	}

	if err := st.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "关闭数据库失败: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "用法: jingzhe [-db data/jingzhe.db] [子命令]")
	fmt.Fprintln(os.Stderr, "子命令:")
	fmt.Fprintln(os.Stderr, "  serve  [-addr :8080]                       常驻服务：调度器 + MCP 接口（默认）")
	fmt.Fprintln(os.Stderr, "  jobs   [-date 20260901]                    演练当日时间线（dry-run，不执行）")
	fmt.Fprintln(os.Stderr, "  config <dump|get KEY|set KEY VALUE>        查看/修改 config_kv（凭据默认掩码）")
	fmt.Fprintln(os.Stderr, "  init   [-capital 元] [-cash 元] [-hold 代码:股数:成本,…]  写入账户基线")
	fmt.Fprintln(os.Stderr, "  run task <任务名> [--date YYYYMMDD]          手工执行单个任务（与到点触发同源）")
	fmt.Fprintln(os.Stderr, "    调度器任务: morning_plan|intraday_scan|evening_pipeline|mail_pending|daily_report")
	fmt.Fprintln(os.Stderr, "    数据面任务: calendar|daily|freshness|screen")
}

// runServe 启动常驻服务：同一进程内并行运行调度器与 MCP 接口。
// 返回进程退出码：0 表示收到信号后正常关停，1 表示有子系统停摆、需要外部重启。
func runServe(ctx context.Context, st *store.Store, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "MCP 服务监听地址")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	cfg, err := config.Load(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	// 令牌优先级：环境变量 > 配置表；为空时 mcp.New 直接拒绝启动。
	apiToken := os.Getenv("JZ_SERVER_API_TOKEN")
	if apiToken == "" {
		apiToken = cfg.GetString("server.api_token")
	}

	rt, err := app.BuildRuntime(ctx, st, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "装配运行时失败: %v\n", err)
		os.Exit(1)
	}
	srv, err := mcp.New(rt.MCPDeps(), apiToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP 服务启动失败: %v\n", err)
		os.Exit(1)
	}

	// 调度器：到点跑当日流水线，并按 run_trace 轨迹补跑与冷却。
	sched := rt.Scheduler().WithOnDone(func(spec scheduler.JobSpec, tradeDate, outcome string, jobErr error) {
		if outcome != model.TraceFail || rt.Alerts == nil {
			return
		}
		reason := ""
		if jobErr != nil {
			reason = jobErr.Error()
		}
		if err := rt.Alerts.Raise(ctx, tradeDate, "scheduler", model.AlertUrgent,
			"JOB_FAILED:"+spec.Name, "任务失败: "+spec.Name, reason); err != nil {
			observability.S().Errorw("落 JOB_FAILED 告警失败", "job", spec.Name, "err", err.Error())
		}
	})

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := sched.Run(runCtx); err != nil {
			observability.S().Errorw("调度器异常退出", "err", err.Error())
			cancel()
		}
	}()
	go func() {
		defer wg.Done()
		err := srv.Start(*addr)
		if errors.Is(err, http.ErrServerClosed) {
			observability.S().Infow("MCP 服务已停止")
			return
		}
		if err != nil {
			observability.S().Errorw("MCP 服务异常退出", "addr", *addr, "err", err.Error())
			cancel()
		}
	}()

	observability.S().Infow("惊蛰常驻服务已启动", "addr", *addr, "jobs", sched.JobCount())

	// 同时等退出信号与子系统停摆：任一子系统不在了，进程就必须结束，
	// 否则会出现「进程活着但没服务」——外部 agent 反而无从发现。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	fatal := false
	select {
	case sig := <-sigCh:
		observability.S().Infow("收到退出信号，优雅关机中", "signal", sig.String())
	case <-runCtx.Done():
		observability.S().Errorw("子系统停摆，服务退出（非零码，需外部重启）", "reason", runCtx.Err())
		fatal = true
	}
	cancel()

	// 关停顺序：先停止接单放掉 HTTP 协程，再等调度器写完在途任务，最后由 main 关库。
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		observability.S().Errorw("MCP 关停未在超时内完成", "err", err.Error())
	}
	wg.Wait()

	if fatal {
		return 1
	}
	return 0
}

// runJobs 演练当日时间线（dry-run，不执行任务、不写 run_trace），用于核对调度配置。
func runJobs(ctx context.Context, st *store.Store, args []string) {
	fs := flag.NewFlagSet("jobs", flag.ExitOnError)
	date := fs.String("date", time.Now().Format("20060102"), "交易日 YYYYMMDD")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	// 与 run 子命令同一份日期校验：错格式的日期会演练出「共 0 次触发」并成功退出，
	// 于是一次拼错的调用被读成「今天没有任务」。
	if err := market.CheckDate(*date); err != nil {
		fmt.Fprintf(os.Stderr, "jobs 的 -date 不可用: %v\n", err)
		os.Exit(2)
	}
	cfg, err := config.Load(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	rt, err := app.BuildRuntime(ctx, st, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "装配运行时失败: %v\n", err)
		os.Exit(1)
	}
	sched := rt.Scheduler().WithDryRun(true)

	lines := sched.SimulateDay(*date)
	fmt.Printf("当日时间线 %s（dry-run，共 %d 次触发）:\n", *date, len(lines))
	for _, l := range lines {
		fmt.Println("  " + l)
	}
}
