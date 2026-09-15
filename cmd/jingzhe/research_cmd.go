// jingzhe research 子命令：回测研究（深历史回补 + 因子 IC 度量）。
//
// 这两件事都**不进生产链路**：回补把多年历史写进库外 CSV.gz（不碰生产库的 45 天窗口），
// IC 是对历史截面的离线统计。它们是"这套因子到底有没有选股能力"的唯一直接证据来源。
//
// 用法:
//
//	jingzhe -db data/jingzhe.db research backfill --from 20240101 --to 20260908 --out data/backtest
//	jingzhe -db data/jingzhe.db research ic --dir data/backtest [--horizon 20] [--step 1]
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"jingzhe-trader/internal/app"
	"jingzhe-trader/internal/backtest"
	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// runResearch 处理 `jingzhe research <backfill|ic>`。
func runResearch(ctx context.Context, st *store.Store, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: jingzhe research <backfill|ic> [flags]")
		os.Exit(2)
	}
	switch args[0] {
	case "backfill":
		researchBackfill(ctx, st, args[1:])
	case "ic":
		researchIC(ctx, st, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "未知 research 子命令: %s（可选 backfill|ic）\n", args[0])
		os.Exit(2)
	}
}

// researchBackfill 回补深历史到库外 CSV.gz。
func researchBackfill(ctx context.Context, st *store.Store, args []string) {
	fs := flag.NewFlagSet("research-backfill", flag.ExitOnError)
	from := fs.String("from", "", "起始交易日 YYYYMMDD（含）")
	to := fs.String("to", "", "结束交易日 YYYYMMDD（含）")
	out := fs.String("out", filepath.Join("data", "backtest"), "输出目录（bars.csv.gz / vals.csv.gz）")
	onlyBars := fs.Bool("only-bars", false, "只补日线（IC 需要估值，一般不要开）")
	proxy := fs.String("proxy", "", "HTTP 代理（拉深历史时的可选出网通道）")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := market.CheckDate(*from); err != nil {
		fmt.Fprintf(os.Stderr, "--from 不可用: %v\n", err)
		os.Exit(2)
	}
	if err := market.CheckDate(*to); err != nil {
		fmt.Fprintf(os.Stderr, "--to 不可用: %v\n", err)
		os.Exit(2)
	}

	cfg, err := config.Load(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	tcli := tushare.NewClient(
		cfg.GetString("tushare.token"),
		cfg.GetString("tushare.base_url"),
		cfg.GetInt("tushare.rate_per_min"),
		tushare.WithProxy(*proxy),
	)
	bf := backtest.NewBackfiller(tcli, func(ctx context.Context) ([]string, error) {
		return st.MarketRepo().TradeDateList(ctx)
	})

	fmt.Printf("开始回补深历史 %s~%s → %s（每日 3 次接口调用，请耐心等待）\n", *from, *to, *out)
	last := ""
	h, err := bf.Run(ctx, backtest.BackfillOptions{
		From: *from, To: *to, OutDir: *out, OnlyBars: *onlyBars,
		Progress: func(date string, bars, done, total int) {
			// 每 10 个交易日或最后一天报一次：逐日刷屏会把配额进度淹掉。
			if done%10 == 0 || done == total {
				fmt.Printf("  [%d/%d] %s 日线 %d 根\n", done, total, date, bars)
			}
			last = date
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "回补失败（截至 %s）: %v\n", last, err)
		os.Exit(1)
	}
	fmt.Printf("回补完成：%d 个交易日、%d 只标的 → %s\n", len(h.Dates), len(h.Codes()), *out)
}

// researchIC 加载历史并输出各因子的 IC 统计。
func researchIC(ctx context.Context, st *store.Store, args []string) {
	fs := flag.NewFlagSet("research-ic", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join("data", "backtest"), "历史目录（bars.csv.gz / vals.csv.gz）")
	horizon := fs.Int("horizon", 0, "只展示指定观察期（交易日；0 = 全部 5/10/20）")
	step := fs.Int("step", 1, "采样步长：每 N 个交易日算一个截面（1 = 每天）")
	minCodes := fs.Int("min-codes", 50, "单截面最小样本数（少于它不参与）")
	universe := fs.String("universe", "screen", "因子计算口径：screen=套用生产硬门槛的可投池；broad=全市场")
	from := fs.String("from", "", "截面起始日 YYYYMMDD（样本外检验用；空=历史起点）")
	to := fs.String("to", "", "截面结束日 YYYYMMDD（样本外检验用；空=历史终点）")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	h, found, err := backtest.Load(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载历史失败: %v\n", err)
		os.Exit(1)
	}
	if !found {
		fmt.Fprintf(os.Stderr, "目录 %s 下没有 bars.csv.gz —— 先跑 research backfill\n", *dir)
		os.Exit(1)
	}
	fmt.Printf("历史：%d 个交易日（%s ~ %s）、%d 只标的\n",
		len(h.Dates), firstDate(h.Dates), lastDate(h.Dates), len(h.Codes()))

	// 配置既决定可投池门槛，也决定当前因子方向；读不到就退出（用错口径的 IC 会误导决策）。
	appCfg, err := config.Load(ctx, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置读取失败（IC 口径需要生产门槛与因子方向）: %v\n", err)
		os.Exit(1)
	}
	mode := appCfg.GetString("screen.factor_mode")
	if !screener.ValidFactorMode(mode) {
		fmt.Fprintf(os.Stderr, "screen.factor_mode=%q 非法（可选 momentum|reversal），请先修正配置\n", mode)
		os.Exit(1)
	}
	fmt.Printf("生产因子方向: screen.factor_mode=%s\n", mode)

	cfg := backtest.DefaultConfig()
	cfg.Step = *step
	cfg.MinCodes = *minCodes
	cfg.From = *from
	cfg.To = *to
	switch *universe {
	case "broad":
		fmt.Println("口径：全市场（未套硬门槛，会被微盘股污染，仅供参考）")
	default:
		fc := app.FilterConfigOf(appCfg)
		cfg.Universe = &fc
		fmt.Printf("口径：可投池近似（流通市值 ≥%.0f万、换手 ≥%.1f%%、价 ≥%.1f、0<PE≤%.0f、0<PB≤%.0f）\n",
			fc.MinCircMvW, fc.MinTurnoverRate, fc.PriceLow, fc.PETtmMax, fc.PBMax)
	}
	res, err := backtest.RunIC(ctx, h, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "IC 计算失败: %v\n", err)
		os.Exit(1)
	}
	printICReport(res, *horizon, mode)
}

// printICReport 输出 IC 报告（含带符号的权重建议）。
//
// 两个综合分并排展示：composite 是**当前配置的方向**，composite_rev 是 IC 反向方向。
// 标签按当前模式生成——写死"当前生产权重"会在默认翻成 reversal 后指向错误的一侧。
func printICReport(res backtest.Result, onlyHorizon int, mode string) {
	// 两个综合分是固定方向，标签标出哪个是当前生效方向；不写死，否则默认一变就指错侧。
	const alt = "（备用，A/B 对照）"
	origLabel, revLabel := "综合分(原始 momentum 权重)", "综合分(IC 反向权重)"
	curComposite := "composite_rev"
	if mode == screener.ModeMomentum {
		origLabel += "（当前生效）"
		revLabel += alt
		curComposite = "composite"
	} else {
		origLabel += alt
		revLabel += "（当前生效）"
	}
	fmt.Printf("参与截面 %d 个\n\n", res.TradeDates)
	for _, hz := range backtest.Horizons {
		if onlyHorizon != 0 && hz != onlyHorizon {
			continue
		}
		fmt.Printf("== 观察期 %d 个交易日（%d 个截面可算）==\n", hz, res.Forward[hz])
		for _, name := range []string{"momentum", "value", "lowvol", "liquidity"} {
			if f, ok := res.Find(name, hz); ok {
				fmt.Println("  " + f.Describe())
			}
		}
		if f, ok := res.Find("composite", hz); ok {
			fmt.Printf("  %s IC=%+.4f  ICIR=%+.3f  n=%d\n", origLabel, f.ICMean, f.ICIR, f.N)
		}
		if f, ok := res.Find("composite_rev", hz); ok {
			fmt.Printf("  %s IC=%+.4f  ICIR=%+.3f  n=%d\n", revLabel, f.ICMean, f.ICIR, f.N)
		}
		if w := res.WeightsFor(hz); len(w) > 0 {
			fmt.Printf("  按 IC **带符号**归一的可选权重（负号=该因子方向与收益相反，应反向使用）: ")
			for _, name := range []string{"momentum", "value", "lowvol", "liquidity"} {
				if v, ok := w[name]; ok {
					fmt.Printf("%s=%+.2f ", name, v)
				}
			}
			fmt.Println()
		} else {
			fmt.Println("  没有任何因子达到可用门槛（|IC|≥0.03 且 |ICIR|≥0.3）")
		}
		fmt.Println()
	}
	// 当前生效方向的综合分 IC 是最该看的一个数：它就是当前选股公式的预测力。
	if f, ok := res.Find(curComposite, 20); ok {
		verdict := "正向可用"
		switch {
		case !f.Usable():
			verdict = "无显著预测力"
		case f.ICMean < 0:
			verdict = "⚠ 反向：当前配置下综合分系统性选到跑输的票"
		}
		fmt.Printf("当前生效方向 %s 20 日 IC=%+.4f ICIR=%+.3f → %s\n", mode, f.ICMean, f.ICIR, verdict)
	}
}

func firstDate(ds []string) string {
	if len(ds) == 0 {
		return ""
	}
	return ds[0]
}

func lastDate(ds []string) string {
	if len(ds) == 0 {
		return ""
	}
	return ds[len(ds)-1]
}
