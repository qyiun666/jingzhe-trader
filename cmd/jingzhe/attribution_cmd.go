// jingzhe attribution 子命令：轻量候选归因（只读 run_trace 与 daily_bar）。
//
// 用法:
//
//	jingzhe attribution [--from 20261009] [--to 20261023] [--horizons 5,10,20] [--top 10]
//
// 默认区间 = to 往前 20 个交易日；horizons 是交易日数，不是自然日。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"jingzhe-trader/internal/attribution"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/store"
)

// runAttribution 处理 `jingzhe attribution`。
func runAttribution(ctx context.Context, st *store.Store, args []string) {
	fs := flag.NewFlagSet("attribution", flag.ExitOnError)
	from := fs.String("from", "", "起始交易日 YYYYMMDD（默认 to 往前 20 个交易日）")
	to := fs.String("to", time.Now().Format("20060102"), "结束交易日 YYYYMMDD")
	horizons := fs.String("horizons", "5,10,20", "前瞻窗口（交易日，逗号分隔）")
	top := fs.Int("top", 10, "行业分层显示前 N 个（0=全部）")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if err := market.CheckDate(*to); err != nil {
		fmt.Fprintf(os.Stderr, "attribution 的 --to 不可用: %v\n", err)
		os.Exit(2)
	}
	hs, err := parseHorizons(*horizons)
	if err != nil {
		fmt.Fprintf(os.Stderr, "attribution 的 --horizons 不可用: %v\n", err)
		os.Exit(2)
	}
	fromDate := strings.TrimSpace(*from)
	if fromDate == "" {
		dates, err := st.ScreenRepo().WindowDates(ctx, *to, 20)
		if err != nil || len(dates) == 0 {
			fmt.Fprintf(os.Stderr, "无法确定默认起始日（%s 及之前没有交易日）：%v\n", *to, err)
			os.Exit(1)
		}
		fromDate = dates[0]
	}
	if err := market.CheckDate(fromDate); err != nil {
		fmt.Fprintf(os.Stderr, "attribution 的 --from 不可用: %v\n", err)
		os.Exit(2)
	}

	rep, err := attribution.Compute(ctx, st, attribution.Options{
		From: fromDate, To: *to, Horizons: hs,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "归因失败: %v\n", err)
		os.Exit(1)
	}
	printAttributionReport(rep, *top)
}

// parseHorizons 解析逗号分隔的前瞻窗口；任何非法项都拒绝，不做"猜一个默认值"。
func parseHorizons(raw string) ([]int, error) {
	parts := strings.Split(raw, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("%q 不是正整数", s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("至少给一个窗口")
	}
	return out, nil
}

func printAttributionReport(rep *attribution.Report, top int) {
	fmt.Printf("候选归因 %s..%s ｜ 决策留痕 %d 条 ｜ 证据行还原 %d 条 ｜ 候选快照 %d 天\n",
		rep.From, rep.To, rep.Decisions, rep.Reconstructed, rep.Snapshots)
	for _, n := range rep.Notes {
		fmt.Printf("  %s\n", n)
	}
	fmt.Println("\n== 全部 ==")
	printHorizonResults(rep.Overall, "  ")

	printBucketSection("按裁决", rep.ByVerdict, 0)
	printBucketSection("按置信度", rep.ByConfidence, 0)
	printBucketSection("按行业", rep.ByIndustry, top)
}

func printBucketSection(title string, buckets []attribution.Bucket, top int) {
	if len(buckets) == 0 {
		return
	}
	fmt.Printf("\n== %s ==\n", title)
	shown := 0
	for _, b := range buckets {
		if top > 0 && shown >= top {
			fmt.Printf("  ……其余 %d 个分组略（--top 0 查看全部）\n", len(buckets)-shown)
			break
		}
		fmt.Printf("  %s（%d 条）\n", b.Key, b.Rows)
		printHorizonResults(b.Results, "    ")
		shown++
	}
}

func printHorizonResults(results []attribution.HorizonResult, indent string) {
	for _, hr := range results {
		if hr.N == 0 {
			fmt.Printf("%s%2d日：暂无可计算样本（待到期 %d、缺数据 %d）\n",
				indent, hr.Horizon, hr.Pending, hr.Missing)
			continue
		}
		ex := "基准不可用"
		if hr.IndexN > 0 {
			ex = fmt.Sprintf("平均超额 %+.2f%% 跑赢 %.0f%%", hr.AvgExcess*100, hr.BeatRate*100)
		}
		fmt.Printf("%s%2d日：n=%d 平均净收益 %+.2f%% %s 胜率 %.0f%%（待到期 %d、缺数据 %d）\n",
			indent, hr.Horizon, hr.N, hr.AvgNet*100, ex, hr.WinRate*100, hr.Pending, hr.Missing)
	}
}
