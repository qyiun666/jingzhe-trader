package factor

import (
	"context"

	"jingzhe-trader/internal/model"
)

// NetProfitYoyFactor 净利润同比增长率因子 (越高越好)
type NetProfitYoyFactor struct{}

// Name 返回因子名称
func (f *NetProfitYoyFactor) Name() string {
	return "netprofit_yoy"
}

// Compute 计算净利润同比增长率因子值
// 取最近一期财报的净利润同比增长率, 越高越好
func (f *NetProfitYoyFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	return finaFactorValues(ctx, date, universe, provider, func(latest *model.FinaIndicator) float64 {
		return latest.NetProfitYoy
	})
}

// RevenueYoyFactor 营收同比增长率因子 (越高越好)
type RevenueYoyFactor struct{}

// Name 返回因子名称
func (f *RevenueYoyFactor) Name() string {
	return "revenue_yoy"
}

// Compute 计算营收同比增长率因子值
// 取最近一期财报的营收同比增长率, 越高越好
func (f *RevenueYoyFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	return finaFactorValues(ctx, date, universe, provider, func(latest *model.FinaIndicator) float64 {
		return latest.ORYoy
	})
}
