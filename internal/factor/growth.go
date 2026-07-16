package factor

import (
	"context"
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
	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		indicators, err := provider.GetFinaIndicator(tsCode)
		if err != nil {
			return nil, err
		}

		// 找到指定日期前最近一期已公告的财报
		latest := findLatestFinaBeforeDate(indicators, date)
		if latest != nil {
			result[tsCode] = latest.NetProfitYoy
		}
	}

	return result, nil
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
	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		indicators, err := provider.GetFinaIndicator(tsCode)
		if err != nil {
			return nil, err
		}

		// 找到指定日期前最近一期已公告的财报
		latest := findLatestFinaBeforeDate(indicators, date)
		if latest != nil {
			result[tsCode] = latest.ORYoy
		}
	}

	return result, nil
}
