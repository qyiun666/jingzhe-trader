package factor

import (
	"context"
	"sort"

	"jingzhe-trader/internal/model"
)

// ROEFactor 净资产收益率因子 (ROE越高越好)
// 取指定日期前最近一期财报的ROE
type ROEFactor struct{}

// Name 返回因子名称
func (f *ROEFactor) Name() string {
	return "roe"
}

// Compute 计算ROE因子值
// ROE越高越好, 直接返回最近一期财报的ROE
func (f *ROEFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		indicators, err := provider.GetFinaIndicator(tsCode)
		if err != nil {
			return nil, err
		}

		// 找到指定日期前最近一期已公告的财报
		latest := findLatestFinaBeforeDate(indicators, date)
		if latest != nil {
			result[tsCode] = latest.ROE
		}
		// 如果没有找到财报数据, 该股票不纳入结果
	}

	return result, nil
}

// GrossMarginFactor 毛利率因子 (越高越好)
type GrossMarginFactor struct{}

// Name 返回因子名称
func (f *GrossMarginFactor) Name() string {
	return "gross_margin"
}

// Compute 计算毛利率因子值
// 毛利率越高越好, 直接返回最近一期财报的毛利率
func (f *GrossMarginFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		indicators, err := provider.GetFinaIndicator(tsCode)
		if err != nil {
			return nil, err
		}

		// 找到指定日期前最近一期已公告的财报
		latest := findLatestFinaBeforeDate(indicators, date)
		if latest != nil {
			result[tsCode] = latest.GrossProfitMargin
		}
	}

	return result, nil
}

// DebtToAssetsFactor 资产负债率因子 (越低越好, 取负值)
type DebtToAssetsFactor struct{}

// Name 返回因子名称
func (f *DebtToAssetsFactor) Name() string {
	return "debt_to_assets"
}

// Compute 计算资产负债率因子值
// 资产负债率越低越好, 所以返回 -debt_to_assets
func (f *DebtToAssetsFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		indicators, err := provider.GetFinaIndicator(tsCode)
		if err != nil {
			return nil, err
		}

		// 找到指定日期前最近一期已公告的财报
		latest := findLatestFinaBeforeDate(indicators, date)
		if latest != nil {
			// 资产负债率越低越好, 取负值
			result[tsCode] = -latest.DebtToAssets
		}
	}

	return result, nil
}

// findLatestFinaBeforeDate 找到指定日期前最近一期已公告的财报
// 比较 ann_date (公告日期), 确保财报在指定日期前已经发布
func findLatestFinaBeforeDate(indicators []model.FinaIndicator, date string) *model.FinaIndicator {
	if len(indicators) == 0 {
		return nil
	}

	// 按公告日期降序排序, 找到第一个在指定日期前的财报
	// 注意: 我们需要按 ann_date 排序, 因为财报可能在报告期后才发布
	sorted := make([]model.FinaIndicator, len(indicators))
	copy(sorted, indicators)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].AnnDate > sorted[j].AnnDate
	})

	for i := range sorted {
		if sorted[i].AnnDate <= date {
			return &sorted[i]
		}
	}

	return nil
}
