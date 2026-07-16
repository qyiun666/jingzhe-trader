package factor

import (
	"context"
	"math"
)

// PEFactor 市盈率因子 (PE越低越好, 价值股特征)
// 取 PE_TTM, 越低分越高, 负PE(亏损)得分最低
type PEFactor struct{}

// Name 返回因子名称
func (f *PEFactor) Name() string {
	return "pe_ttm"
}

// Compute 计算PE因子值
// 因子值越高越好, 所以返回 -PE_TTM (PE越低, 负值越大, 分数越高)
// 负PE(亏损)的股票返回最小的负值 (即最低分)
func (f *PEFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	basics, err := provider.GetDailyBasic(date)
	if err != nil {
		return nil, err
	}

	// 构建 universe 的 map 用于快速查找
	universeMap := make(map[string]bool, len(universe))
	for _, code := range universe {
		universeMap[code] = true
	}

	result := make(map[string]float64, len(universe))
	for _, b := range basics {
		if !universeMap[b.TsCode] {
			continue
		}
		pe := b.PE_TTM
		if pe <= 0 {
			// 亏损股PE为负, 给予最低分 (用一个很小的负数表示)
			result[b.TsCode] = math.Inf(-1)
		} else {
			// PE越低越好, 取倒数或取负值
			// 这里取 -PE, PE越小, -PE越大, 排名越靠前
			result[b.TsCode] = -pe
		}
	}

	return result, nil
}

// PBFactor 市净率因子 (PB越低越好)
type PBFactor struct{}

// Name 返回因子名称
func (f *PBFactor) Name() string {
	return "pb"
}

// Compute 计算PB因子值
// PB越低越好, 返回 -PB
func (f *PBFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	basics, err := provider.GetDailyBasic(date)
	if err != nil {
		return nil, err
	}

	// 构建 universe 的 map 用于快速查找
	universeMap := make(map[string]bool, len(universe))
	for _, code := range universe {
		universeMap[code] = true
	}

	result := make(map[string]float64, len(universe))
	for _, b := range basics {
		if !universeMap[b.TsCode] {
			continue
		}
		pb := b.PB
		if pb <= 0 {
			// 负PB的股票 (资不抵债), 给予最低分
			result[b.TsCode] = math.Inf(-1)
		} else {
			// PB越低越好, 取负值
			result[b.TsCode] = -pb
		}
	}

	return result, nil
}

// DividendYieldFactor 股息率因子 (越高越好)
type DividendYieldFactor struct{}

// Name 返回因子名称
func (f *DividendYieldFactor) Name() string {
	return "dividend_yield"
}

// Compute 计算股息率因子值
// 股息率越高越好, 直接返回 DV_RATIO
func (f *DividendYieldFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	basics, err := provider.GetDailyBasic(date)
	if err != nil {
		return nil, err
	}

	// 构建 universe 的 map 用于快速查找
	universeMap := make(map[string]bool, len(universe))
	for _, code := range universe {
		universeMap[code] = true
	}

	result := make(map[string]float64, len(universe))
	for _, b := range basics {
		if !universeMap[b.TsCode] {
			continue
		}
		result[b.TsCode] = b.DV_RATIO
	}

	return result, nil
}
