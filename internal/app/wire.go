package app

import (
	"fmt"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// Options 应用装配参数（依赖注入入口）。
type Options struct {
	Store  *store.Store
	Config *config.Config
}

// Wire 组装顶层 Application（Batch 2：挂接 tushare 客户端、实时行情源、数据接入器）。
//
// 实时行情源链路：gotdx 主源 + 腾讯降级备用源（主源不可达/失败自动切换）。
func Wire(opts Options) (*Application, error) {
	if opts.Store == nil {
		return nil, fmt.Errorf("app.Wire: Store 不能为空")
	}
	if opts.Config == nil {
		return nil, fmt.Errorf("app.Wire: Config 不能为空")
	}

	tcli := tushare.NewClient(
		opts.Config.GetString("tushare.token"),
		opts.Config.GetString("tushare.base_url"),
		opts.Config.GetInt("tushare.rate_per_min"),
	)

	// gotdx 主源 + 腾讯降级备用源
	q := quote.NewGotdxSource(quote.NewTencentSource())

	return &Application{
		store:      opts.Store,
		config:     opts.Config,
		tushare:    tcli,
		quote:      q,
		dataloader: dataloader.New(opts.Store, tcli),
	}, nil
}
