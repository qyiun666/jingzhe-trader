// jingzhe init：把账户基线写进库（本金 / 持仓 / 可用资金），并回显账本算出来的口径。
//
// 它不是第二套记账逻辑：全部走 ticket.Ledger.SyncPortfolio（与 MCP sync_portfolio 同一个实现），
// CLI 只把命令行参数翻成一次组合同步，再把同步后的账户状态打出来。
// 顺带把 config_kv 补齐默认值（配置的唯一数据源是库，不是 .env）。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

const initActor = "jingzhe-init"

// runInit 处理 `jingzhe init [-date YYYYMMDD] [-capital 元] [-cash 元] [-hold 代码:股数:成本价,...]`。
func runInit(ctx context.Context, st *store.Store, args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	date := fs.String("date", time.Now().Format("20060102"), "账户口径所属交易日 YYYYMMDD")
	capitalYuan := fs.Float64("capital", 0, "本金 = 期初总资产（元，含持仓成本）；write-once，已配置时不覆盖")
	cashYuan := fs.Float64("cash", -1, "券商口径的可用现金（元）；省略则按 本金 − 持仓成本 推算")
	holdSpec := fs.String("hold", "", "当前持仓：代码:股数:成本价，多只用逗号分隔，如 601233.SH:200:26.40")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if err := config.NewRepo(st).SeedDefaults(ctx); err != nil {
		fatal("init", fmt.Errorf("补齐默认配置失败: %w", err))
	}
	items, err := parseHoldings(*holdSpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init 失败: %v\n", err)
		os.Exit(2)
	}
	capital := model.FromFloat(*capitalYuan)
	cash, err := cashOf(capital, *cashYuan, items)
	if err != nil {
		fmt.Fprintf(os.Stderr, "init 失败: %v\n", err)
		os.Exit(2)
	}

	// 成本参数在这里用不上：Ledger 的费率只在 ReportFill 里生效，init 不落成交。
	led := ticket.NewLedger(st, market.CostParams{}, capital)
	synced, rejected, err := led.SyncPortfolio(ctx, ticket.PortfolioSync{
		Date: *date, Capital: capital, Cash: cash, Items: items, Actor: initActor,
	})
	if err != nil {
		fatal("init", err)
	}
	printAccountState(ctx, st, led, synced, rejected)
}

// parseHoldings 解析 -hold "代码:股数:成本价,…"。价格为元、数量为股，与 MCP 入参同口径。
// 校准进来的持仓默认全部可卖（不是今天买的，不受 T+1 约束）。
func parseHoldings(spec string) ([]ticket.PortfolioInput, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	out := make([]ticket.PortfolioInput, 0, 4)
	for i, part := range strings.Split(spec, ",") {
		fields := strings.Split(strings.TrimSpace(part), ":")
		if len(fields) != 3 {
			return nil, fmt.Errorf("第 %d 个持仓 %q 不是 代码:股数:成本价 的格式", i+1, part)
		}
		qty, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil || qty <= 0 {
			return nil, fmt.Errorf("第 %d 个持仓的股数非法: %q", i+1, fields[1])
		}
		price, err := strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		if err != nil || price <= 0 {
			return nil, fmt.Errorf("第 %d 个持仓的成本价非法: %q", i+1, fields[2])
		}
		out = append(out, ticket.PortfolioInput{
			TsCode: strings.TrimSpace(fields[0]), TotalQty: model.Qty(qty), AvailableQty: model.Qty(qty),
			CostPrice: model.FromFloat(price),
		})
	}
	return out, nil
}

// cashOf 可用现金：给了 -cash 就用它；否则 本金 − Σ持仓成本。
func cashOf(capital model.Fen, cashYuan float64, items []ticket.PortfolioInput) (model.Fen, error) {
	if cashYuan >= 0 {
		return model.FromFloat(cashYuan), nil
	}
	var cost model.Fen
	for _, it := range items {
		cost += it.CostPrice.Mul(it.TotalQty)
	}
	if capital <= 0 {
		return 0, fmt.Errorf("首次初始化要么给 -capital（本金=期初总资产），要么给 -cash（可用现金）")
	}
	if capital < cost {
		return 0, fmt.Errorf("本金 %s 元小于持仓成本 %s 元：口径矛盾，请核对券商数字", capital, cost)
	}
	return capital - cost, nil
}

// printAccountState 回显同步结果与账本算出来的三个数（init 有没有跑通以此为准）。
func printAccountState(ctx context.Context, st *store.Store, led *ticket.Ledger, synced int, rejected bool) {
	cash, err := led.Cash(ctx)
	if err != nil {
		fatal("init", fmt.Errorf("读取可用现金失败: %w", err))
	}
	positions, err := st.TradeRepo().ListPositions(ctx)
	if err != nil {
		fatal("init", fmt.Errorf("读取持仓失败: %w", err))
	}
	var mv model.Fen
	var count int
	for _, p := range positions {
		if p.TotalQty <= 0 {
			continue // 清仓后的历史行不算持仓
		}
		count++
		cost := p.CostPrice.Mul(p.TotalQty)
		mv += cost
		fmt.Printf("  持仓 %s %d 股 成本 %s 元/股 ｜ 可卖 %d 股 ｜ 成本合计 %s 元\n",
			p.TsCode, int64(p.TotalQty), p.CostPrice, int64(p.Available()), cost)
	}
	fmt.Printf("校准持仓 %d 只；账本口径：可用资金 %s 元 ｜ 持仓成本 %s 元 ｜ 总资产(成本) %s 元\n",
		synced, cash, mv, cash+mv)
	if count != synced {
		fmt.Printf("注意：账本里共 %d 只非零持仓，本次只校准了 %d 只\n", count, synced)
	}
	if rejected {
		fmt.Println("注意：本金为 write-once，本次未覆盖（现有本金保持不变，如需修正请走人工复核）")
	}
}
