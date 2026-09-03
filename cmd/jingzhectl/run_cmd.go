// jingzhectl run 子命令：任务编排（数据接入 + 选股 + 信号）。
//
// 用法:
//   jingzhectl -db data/jingzhe.db run task calendar
//   jingzhectl -db data/jingzhe.db run task daily  --date 20260901 [--back 10]
//   jingzhectl -db data/jingzhe.db run task fina   [--limit 100]
//   jingzhectl -db data/jingzhe.db run task freshness --date 20260901
//   jingzhectl -db data/jingzhe.db run task screener  --date 20260901
//   jingzhectl -db data/jingzhe.db run task signal    --date 20260901
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
)

// runRun 处理 `jingzhectl run task <calendar|daily|fina|freshness|screener|signal> [flags]`。
//
// 装配流程复用现有 config + store（main 已打开 store），经 app.Wire 注入
// tushare 客户端 / 实时行情源 / 数据接入器，再分派到对应任务。
func runRun(ctx context.Context, st *store.Store, args []string) {
	if len(args) < 2 || args[0] != "task" {
		fmt.Fprintln(os.Stderr, "用法: jingzhectl run task <calendar|daily|fina|freshness|screener|signal> [--date YYYYMMDD] [--limit N] [--back N]")
		os.Exit(2)
	}
	task := args[1]

	fs := flag.NewFlagSet("run-task", flag.ExitOnError)
	date := fs.String("date", "", "交易日 YYYYMMDD（daily/freshness/screener/signal 任务必填）")
	limit := fs.Int("limit", 0, "财务同步处理上限（fina 任务；0=不限）")
	back := fs.Int("back", 10, "自动回补前 N 个交易日（daily 任务）")
	if err := fs.Parse(args[2:]); err != nil {
		os.Exit(2)
	}

	cfg, err := config.Load(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	application, err := app.Wire(app.Options{Store: st, Config: cfg})
	if err != nil {
		fmt.Fprintf(os.Stderr, "装配应用失败: %v\n", err)
		os.Exit(1)
	}

	switch task {
	case "calendar":
		if err := application.RunTask(ctx, "calendar", "", 0, 0); err != nil {
			fmt.Fprintf(os.Stderr, "calendar 任务失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("calendar 同步完成")

	case "daily":
		if *date == "" {
			fmt.Fprintln(os.Stderr, "daily 任务需要 --date YYYYMMDD")
			os.Exit(2)
		}
		if err := application.RunTask(ctx, "daily", *date, 0, *back); err != nil {
			fmt.Fprintf(os.Stderr, "daily 任务失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("daily 同步完成（%s，回补 %d 个交易日）\n", *date, *back)

	case "fina":
		if err := application.RunTask(ctx, "fina", "", *limit, 0); err != nil {
			fmt.Fprintf(os.Stderr, "fina 任务失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("fina 同步完成（limit=%d）\n", *limit)

	case "freshness":
		if *date == "" {
			fmt.Fprintln(os.Stderr, "freshness 任务需要 --date YYYYMMDD")
			os.Exit(2)
		}
		rep, err := application.FreshnessCheck(ctx, *date)
		if err != nil {
			fmt.Fprintf(os.Stderr, "freshness 检查失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(rep.String())
		// 非交易日（跳过）不视为失败；不新鲜则非零退出，便于 CI/看门狗判读。
		if !rep.Fresh && !rep.Skipped {
			os.Exit(1)
		}

	case "screener":
		if *date == "" {
			fmt.Fprintln(os.Stderr, "screener 任务需要 --date YYYYMMDD")
			os.Exit(2)
		}
		if !gateOrExit(ctx, st, cfg, "screener", *date) {
			os.Exit(1)
		}
		rep, err := screener.New(st, filterConfigFrom(cfg)).Run(ctx, *date)
		if err != nil {
			fmt.Fprintf(os.Stderr, "screener 任务失败: %v\n", err)
			os.Exit(1)
		}
		printScreenerReport(rep)
		if rep.Empty {
			os.Exit(3) // 候选为空：告警与观察名单已落库，非零退出便于看门狗判读
		}

	case "signal":
		if *date == "" {
			fmt.Fprintln(os.Stderr, "signal 任务需要 --date YYYYMMDD")
			os.Exit(2)
		}
		if !gateOrExit(ctx, st, cfg, "signal", *date) {
			os.Exit(1)
		}
		rp, gear := riskParamsFrom(ctx, cfg, st)
		svc := signal.NewService(st, initialCapitalOf(cfg), costParamsFrom(cfg))
		rep, err := svc.Generate(ctx, *date, rp, gear, signal.PassThroughConfirmer{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "signal 任务失败: %v\n", err)
			os.Exit(1)
		}
		printSignalReport(rep)

	default:
		fmt.Fprintf(os.Stderr, "未知 run 任务: %s（支持 calendar/daily/fina/freshness/screener/signal）\n", task)
		os.Exit(2)
	}
}

// gateOrExit 运行数据新鲜度门禁；不新鲜时落 DATA_STALE 告警并返回 false（§5.1：不生成任何指令）。
func gateOrExit(ctx context.Context, st *store.Store, cfg *config.Config, task, date string) bool {
	rep, err := dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows")).Check(ctx, date)
	if err != nil {
		fmt.Fprintf(os.Stderr, "新鲜度门禁执行失败: %v\n", err)
		return false
	}
	if rep.Skipped {
		fmt.Println("非交易日，任务跳过")
		return false
	}
	if !rep.Fresh {
		fmt.Printf("数据不新鲜，%s 任务终止：\n%s\n", task, rep.String())
		if err := st.OpsRepo().RaiseAlert(ctx, model.AgentAlert{
			TradeDate: date,
			Source:    task,
			Level:     model.AlertUrgent,
			Code:      "DATA_STALE",
			Title:     "数据不新鲜，任务终止",
			Content:   rep.String(),
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "落 DATA_STALE 告警失败: %v\n", err)
		}
		return false
	}
	return true
}

// filterConfigFrom 从配置构建粗筛参数。
func filterConfigFrom(cfg *config.Config) screener.FilterConfig {
	fc := screener.DefaultFilterConfig()
	if v := cfg.GetInt("screen.top_n"); v > 0 {
		fc.TopN = v
	}
	if v := cfg.GetFloat("screen.min_circ_mv_w"); v > 0 {
		fc.MinCircMvW = v
	}
	if v := cfg.GetFloat("screen.min_turnover_rate"); v > 0 {
		fc.MinTurnoverRate = v
	}
	if v := cfg.GetFloat("screen.price_low"); v > 0 {
		fc.PriceLow = v
	}
	if v := cfg.GetFloat("screen.price_high"); v > 0 {
		fc.PriceHigh = v
	}
	if v := cfg.GetFloat("screen.pe_ttm_max"); v > 0 {
		fc.PETtmMax = v
	}
	if v := cfg.GetFloat("screen.pb_max"); v > 0 {
		fc.PBMax = v
	}
	return fc
}

// riskParamsFrom 组装生效风控参数：档位/锁利取自 goal_state（缺省 G1/未锁利），
// 总资产取最新快照（缺省回落 config 本金），落后策略取 goal.pace_policy。
func riskParamsFrom(ctx context.Context, cfg *config.Config, st *store.Store) (risk.RiskParams, model.Gear) {
	gear := model.GearG1
	lock := false
	if gs, err := st.GoalRepo().GetGoalState(ctx); err == nil && gs.CurrentGear.Valid() {
		gear = gs.CurrentGear
		lock = gs.ProfitLock
	}
	totalAsset := initialCapitalOf(cfg)
	if sn, err := st.TradeRepo().LatestSnapshot(ctx); err == nil && sn.TotalAsset > 0 {
		totalAsset = sn.TotalAsset
	}
	base := risk.DefaultBase(totalAsset)
	if v := cfg.GetFloat("risk.max_sector_pct"); v > 0 {
		base.MaxSectorPct = v
	}
	if v := cfg.GetFloat("risk.take_profit_pct"); v > 0 {
		base.TakeProfitPct = v
	}
	var pace risk.PaceAdjust = risk.NoPace{}
	switch cfg.GetString("goal.pace_policy") {
	case "unrestricted":
		pace = risk.UnrestrictedPace{}
	case "conservative":
		pace = risk.ConservativePace{}
	}
	return risk.Resolve(base, gear, lock, pace), gear
}

// initialCapitalOf 本金（config account.initial_capital，单位元）；未配置按 1 万元回落。
func initialCapitalOf(cfg *config.Config) model.Fen {
	if v := cfg.GetFloat("account.initial_capital"); v > 0 {
		return model.FromFloat(v)
	}
	return model.FromFloat(10000)
}

// costParamsFrom 交易成本参数（config cost.* 键）。
func costParamsFrom(cfg *config.Config) market.CostParams {
	return market.CostParams{
		CommissionRate:  cfg.GetFloat("cost.commission_rate"),
		MinCommission:   model.FromFloat(cfg.GetFloat("cost.min_commission")),
		StampTaxRate:    cfg.GetFloat("cost.stamp_tax_rate"),
		TransferFeeRate: cfg.GetFloat("cost.transfer_fee_rate"),
	}
}

// printScreenerReport 打印选股漏斗与候选（诊断不黑盒）。
func printScreenerReport(rep *screener.Report) {
	fmt.Printf("选股完成 %s：打分样本 %d 只\n", rep.TradeDate, rep.ScoredTotal)
	fmt.Println("漏斗：")
	for _, f := range rep.FunnelRows {
		fmt.Printf("  [%d] %-24s %6d → %6d（淘汰 %d）%s\n",
			f.Stage, f.StageName, f.PassedIn, f.PassedOut, f.Dropped, f.DropReasons)
	}
	if rep.Empty {
		fmt.Printf("候选 0 条！已落 SCREEN_EMPTY urgent 告警，观察名单 %d 只：\n", len(rep.WatchRows))
		for _, w := range rep.WatchRows {
			fmt.Printf("  #%02d %s 得分 %.1f %s\n", w.Rank, w.TsCode, w.Score, w.Reason)
		}
		return
	}
	fmt.Printf("候选 Top%d：\n", len(rep.Candidates))
	for _, c := range rep.Candidates {
		fmt.Printf("  #%02d %s 收盘 %s 分 %.1f [动量%.0f 质量%.0f 价值%.0f 低波%.0f 流动%.0f]\n      %s\n",
			c.Rank, c.TsCode, c.Close, c.Score, c.F_Momentum, c.F_Quality, c.F_Value, c.F_LowVol, c.F_Liquidity, c.Reason)
	}
}

// printSignalReport 打印信号生成结果。
func printSignalReport(rep *signal.Report) {
	fmt.Printf("信号完成 %s：候选 %d，买入信号 %d，卖出信号 %d，新增落库 %d（重跑幂等），否决 %d，指令单 %d\n",
		rep.TradeDate, rep.Candidates, rep.BuySignals, rep.SellSignals, rep.Inserted, rep.Rejected, rep.Tickets)
	for _, note := range rep.Notes {
		fmt.Printf("  提示: %s\n", note)
	}
	for _, r := range rep.Rejections {
		fmt.Printf("  否决 %s [%s] %s\n", r.TsCode, r.Rule, r.Msg)
	}
	if rep.Empty {
		fmt.Println("候选池为空（选股阶段已告警），未生成任何信号")
	}
}
