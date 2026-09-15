package backtest

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/tushare"
)

// Backfiller 深历史回补器：从 Tushare 逐日拉全市场日线/估值/复权因子，写进 CSV.gz。
//
// 为什么走库外文件而不进 SQLite：生产库的 daily_bar 只保留 45 天（操作数据），
// 回测需要数年历史。把历史灌进主库既会让保留策略与回测需求冲突（每天删的正是
// 回测要的），也会把 NAS 上的小库撑大。两个 CSV.gz 就够 IC 用，且可随时删掉重拉。
//
// 调用量估算：每天 3 次接口调用（daily / daily_basic / adj_factor），一年约 730 次，
// 在 200 次/分钟的配额下约 4 分钟；按交易日全市场拉，是覆盖全市场最省的路径。
type Backfiller struct {
	client *tushare.Client
	// calendar 返回升序交易日列表。由调用方注入（通常是 store.MarketRepo().TradeDateList），
	// 这样 backtest 无需 import store，测试也能喂一段人造日历。
	calendar func(context.Context) ([]string, error)
}

// NewBackfiller 构造回补器。calendar 为交易日列表读取函数，不可为空。
func NewBackfiller(client *tushare.Client, calendar func(context.Context) ([]string, error)) *Backfiller {
	return &Backfiller{client: client, calendar: calendar}
}

// BackfillOptions 回补参数。
type BackfillOptions struct {
	From     string // 起始交易日 YYYYMMDD（含）
	To       string // 结束交易日 YYYYMMDD（含）
	OutDir   string // 输出目录（写 bars.csv.gz / vals.csv.gz）；空 = 只算不落盘
	OnlyBars bool   // 只补日线，跳过估值与复权（IC 需要估值，一般不要开）
	Progress func(date string, bars, done, total int)
}

// Run 执行回补：逐交易日拉全市场 → 复权 → 累积 → 一次性落两个 CSV.gz。
//
// 失败即整体失败（不写半截文件）：缺口在 IC 里会表现为"某些截面收益算不出来"，
// 那种缺失很难归因，不如当场报错让人补。
func (b *Backfiller) Run(ctx context.Context, opts BackfillOptions) (*History, error) {
	if b == nil || b.calendar == nil {
		return nil, fmt.Errorf("回补器未装配交易日历读取函数")
	}
	if opts.From == "" || opts.To == "" {
		return nil, fmt.Errorf("回补需指定起止日期")
	}
	if opts.From > opts.To {
		return nil, fmt.Errorf("起始日 %s 晚于结束日 %s", opts.From, opts.To)
	}
	days, err := b.tradeDates(ctx, opts.From, opts.To)
	if err != nil {
		return nil, err
	}
	if len(days) == 0 {
		return nil, fmt.Errorf("区间 %s~%s 没有交易日（先跑 calendar 补齐日历）", opts.From, opts.To)
	}

	h := NewHistory()
	for i, date := range days {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := b.oneDay(ctx, h, date, opts.OnlyBars)
		if err != nil {
			return nil, fmt.Errorf("回补 %s 失败: %w", date, err)
		}
		if opts.Progress != nil {
			opts.Progress(date, n, i+1, len(days))
		}
	}
	h.Finalize()
	if opts.OutDir != "" {
		files, err := h.Save(opts.OutDir)
		if err != nil {
			return nil, err
		}
		observability.S().Infow("回测历史已落盘",
			"from", opts.From, "to", opts.To, "dates", len(h.Dates),
			"codes", len(h.Codes()), "files", files)
	}
	return h, nil
}

// oneDay 拉取单个交易日的全市场日线（+ 估值/复权）。返回该日入历史的日线根数。
func (b *Backfiller) oneDay(ctx context.Context, h *History, date string, onlyBars bool) (int, error) {
	bars, err := b.client.Daily(ctx, date)
	if err != nil {
		return 0, err
	}
	adj := map[string]float64{}
	if !onlyBars {
		adjs, err := b.client.AdjFactor(ctx, date)
		if err != nil {
			return 0, err
		}
		for _, a := range adjs {
			adj[a.TsCode] = a.AdjFactor
		}
	}

	h.MarkDate(date)
	n := 0
	for i := range bars {
		bd := &bars[i]
		f := adj[bd.TsCode]
		if onlyBars {
			f = 1
		}
		if f == 0 {
			continue // 缺复权因子不写：混进原始价会让序列凭空跳水（与生产 dataloader 同判据）
		}
		raw := bd.Close.Float()
		h.AddBar(bd.TsCode, date, PricePoint{Close: raw * f, VolLot: bd.VolLot, RawClose: raw})
		n++
	}
	if onlyBars {
		return n, nil
	}

	dbs, err := b.client.DailyBasic(ctx, date)
	if err != nil {
		return 0, err
	}
	for _, v := range dbs {
		h.AddValuation(v.TsCode, date, ValPoint{
			TurnoverRate: v.TurnoverRate, PETtm: v.PETtm, PB: v.PB, CircMvW: v.CircMvW,
		})
	}
	return n, nil
}

// tradeDates 从注入的交易日历取 [from, to] 区间内的交易日。
func (b *Backfiller) tradeDates(ctx context.Context, from, to string) ([]string, error) {
	all, err := b.calendar(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取交易日历失败: %w", err)
	}
	var out []string
	for _, d := range all {
		if d >= from && d <= to {
			out = append(out, d)
		}
	}
	return out, nil
}
