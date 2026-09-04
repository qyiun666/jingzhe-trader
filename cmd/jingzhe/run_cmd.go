// jingzhe run 子命令：手工执行单个任务。
//
// 任务名分两组，`run task` 两组都接受：
//
//	调度器注册名（与 serve 到点触发同一份、同一条 runJob 路径）：
//	  morning_plan / intraday_scan / evening_pipeline / mail_pending / daily_report
//	CLI 独有的数据面任务（没有到点触发，只用于接入与排查）：
//	  calendar / daily / freshness / screen
//
// 用法:
//
//	jingzhe -db data/jingzhe.db run task calendar
//	jingzhe -db data/jingzhe.db run task daily     --date 20260901 [--back N]
//	jingzhe -db data/jingzhe.db run task freshness --date 20260901
//	jingzhe -db data/jingzhe.db run task screen    --date 20260901
//	jingzhe -db data/jingzhe.db run task evening_pipeline --date 20260901
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/scheduler"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
)

// dataTasks 只有 CLI 提供、不在调度器注册表里的数据面任务。
var dataTasks = []string{"calendar", "daily", "freshness", "screen"}

// runRun 处理 `jingzhe run task <name> [flags]`。
//
// 装配与常驻服务完全同源（app.BuildRuntime），两组任务名之外的每一个任务都直接走调度器的
// runJob 路径：手工跑出来的漏斗、风控参数与指令单必须与到点自动跑的一致，否则手工复现失去意义。
func runRun(ctx context.Context, st *store.Store, args []string) {
	if len(args) < 2 || args[0] != "task" {
		fmt.Fprintln(os.Stderr, "用法: jingzhe run task <任务名> [--date YYYYMMDD] [--back N]")
		fmt.Fprintln(os.Stderr, "  数据面任务: [calendar daily freshness screen]（calendar 外均需 --date）")
		fmt.Fprintln(os.Stderr, "  调度器任务: [morning_plan intraday_scan evening_pipeline mail_pending daily_report]（均需 --date）")
		os.Exit(2)
	}
	task := args[1]

	fs := flag.NewFlagSet("run-task", flag.ExitOnError)
	date := fs.String("date", "", "交易日 YYYYMMDD（除 calendar 外全部必填）")
	back := fs.Int("back", 0, "回补前 N 个交易日（daily 任务；0=按选股窗口自动定）")
	if err := fs.Parse(args[2:]); err != nil {
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

	switch task {
	case "calendar":
		fatal(task, rt.Dataloader.SyncCalendar(ctx))
		fmt.Println("calendar 同步完成")

	case "daily":
		needDate(task, *date)
		days := *back
		if days <= 0 {
			days = rt.Screener.SyncBackDays()
		}
		fatal(task, rt.Dataloader.SyncDaily(ctx, *date, days))
		fmt.Printf("daily 同步完成（%s，覆盖最近 %d 个交易日）\n", *date, days+1)

	case "freshness":
		needDate(task, *date)
		rep, err := rt.Freshness.Check(ctx, *date)
		fatal(task, err)
		fmt.Print(rep.String())
		// 非交易日（跳过）不视为失败；不新鲜则非零退出，便于 agent 判读。
		if !rep.Fresh && !rep.Skipped {
			os.Exit(1)
		}

	case "screen":
		needDate(task, *date)
		requireFreshGate(ctx, rt, task, *date)
		budget, err := screenBudgetOf(ctx, rt, *date)
		fatal(task, err)
		rep, err := rt.Screener.Run(ctx, *date, budget)
		fatal(task, err)
		printScreenerReport(rep)
		if rep.Empty {
			os.Exit(3) // 候选 0 条：告警已落库，非零退出便于 agent 判读
		}

	default:
		// 其余任务名一律按调度器注册表处理：与到点触发走同一条 runJob 路径，
		// 这样手工补跑与自动跑出来的漏斗、指令单、当日轨迹完全同源。
		if !hasName(rt.TaskNames(), task) {
			fmt.Fprintf(os.Stderr, "未知 run 任务: %s\n调度器任务: %v\n数据面任务: %v\n",
				task, rt.TaskNames(), dataTasks)
			os.Exit(2)
		}
		needDate(task, *date)
		if err := rt.RunTaskOnce(ctx, task, *date, "manual"); err != nil {
			fmt.Fprintf(os.Stderr, "%s 任务失败: %v\n", task, err)
			if task == jobEveningPipeline {
				printTickets(ctx, st, *date)
			}
			os.Exit(1)
		}
		fmt.Printf("%s 完成（%s）\n", task, *date)
		if task == jobEveningPipeline {
			printTickets(ctx, st, *date)
		}
	}
}

// jobEveningPipeline 收盘后整链任务的注册名（与 scheduler.BuildJobs 一致）。
const jobEveningPipeline = "evening_pipeline"

// hasName 任务名是否在注册表里。
func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// screenBudgetOf 手工试跑漏斗的资金与大盘口径：直接复用调度器的 ScreenBudget。
// 判据失败即退出——手工复现的全部意义在于与到点自动跑的一致，缺一项口径就不可比。
func screenBudgetOf(ctx context.Context, rt *app.Runtime, date string) (screener.Budget, error) {
	rp, _, err := rt.Goal.RiskParams(ctx, date)
	if err != nil {
		return screener.Budget{}, fmt.Errorf("读取风控参数失败: %w", err)
	}
	return scheduler.ScreenBudget(ctx, rt.Store, rt.Ledger, date, rp)
}

// fatal 统一的任务失败出口（打印带任务名的原因并以 1 退出）。
func fatal(task string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s 任务失败: %v\n", task, err)
		os.Exit(1)
	}
}

// needDate 校验日期型任务的必填参数。
func needDate(task, date string) {
	if date == "" {
		fmt.Fprintf(os.Stderr, "%s 任务需要 --date YYYYMMDD\n", task)
		os.Exit(2)
	}
	if err := market.CheckDate(date); err != nil {
		fmt.Fprintf(os.Stderr, "%s 的 --date 不可用: %v\n", task, err)
		os.Exit(2)
	}
}

// requireFreshGate 运行数据新鲜度门禁；不新鲜时落 DATA_STALE 告警并终止任务（不生成任何指令）。
func requireFreshGate(ctx context.Context, rt *app.Runtime, task, date string) {
	rep, err := rt.Freshness.Check(ctx, date)
	if err != nil {
		fatal(task, fmt.Errorf("新鲜度门禁执行失败: %w", err))
	}
	if rep.Skipped {
		fmt.Println("非交易日，任务跳过")
		os.Exit(0)
	}
	if !rep.Fresh {
		fmt.Printf("数据不新鲜，%s 任务终止：\n%s\n", task, rep.String())
		if err := rt.Alerts.Raise(ctx, date, task, model.AlertUrgent,
			"DATA_STALE", "数据不新鲜，任务终止", rep.String()); err != nil {
			fmt.Fprintf(os.Stderr, "落 DATA_STALE 告警失败: %v\n", err)
		}
		os.Exit(1)
	}
}

// printScreenerReport 打印板块排名、漏斗每级与候选（诊断不黑盒；全程只读，不落库）。
func printScreenerReport(rep *screener.Report) {
	fmt.Printf("选股完成 %s：打分样本 %d 只\n", rep.TradeDate, rep.ScoredTotal)
	for _, n := range rep.Notes {
		fmt.Printf("  %s\n", n)
	}
	fmt.Println("漏斗：")
	for _, st := range rep.Stages {
		fmt.Printf("  [%d] %-22s %6d → %6d  %s\n", st.Stage, st.Name, st.In, st.Out, dropText(st.Drops))
	}
	if rep.Empty {
		fmt.Println("候选 0 条：已落 SCREEN_EMPTY urgent 告警（卡在哪一级见上方漏斗）")
		return
	}
	fmt.Printf("候选 Top%d：\n", len(rep.Candidates))
	for _, c := range rep.Candidates {
		fmt.Printf("  #%02d %s %s 收盘 %s元｜评分 %.1f [动量%.0f 价值%.0f 低波%.0f 流动%.0f] 板块 %s(%+.1f%%)\n      %s\n",
			c.Rank, c.TsCode, c.Name, c.Close, c.Score,
			c.Factors.Momentum, c.Factors.Value, c.Factors.LowVol, c.Factors.Liquidity,
			c.Industry, c.SectorMom*100, c.Reason)
	}
}

// dropText 淘汰原因分布（CLI 摘要）。
func dropText(drops map[string]int) string {
	out := ""
	for k, v := range drops {
		out += fmt.Sprintf(" %s=%d", k, v)
	}
	return out
}

// printTickets 打印当日待买卖表（选股与决策的唯一落库结果）。
func printTickets(ctx context.Context, st *store.Store, date string) {
	tks, err := st.TradeRepo().ListTickets(ctx, date, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取指令单失败: %v\n", err)
		return
	}
	fmt.Printf("当日指令单 %d 张：\n", len(tks))
	for _, t := range tks {
		fmt.Printf("  #%d %s %s %s 数量 %d 参考价 %.2f 状态 %s\n      %s\n",
			t.ID, t.Direction, t.TsCode, t.Name, int64(t.Qty), float64(t.RefPrice)/100, t.Status, t.Reason)
	}
}
