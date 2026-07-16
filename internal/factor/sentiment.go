package factor

import (
	"context"
	"fmt"
)

// TurnoverFactor 换手率因子 (最近N日平均换手率, 越高越好)
// 高换手率说明市场关注度高、流动性好
// 方向: 正向 (换手率越高得分越高)
type TurnoverFactor struct {
	Period int // 回看周期, 默认20日
}

// Name 返回因子名称
func (f *TurnoverFactor) Name() string {
	return fmt.Sprintf("turnover_%d", f.Period)
}

// Compute 计算换手率因子值
// 取最近 period 个交易日的平均换手率, 越高越好
func (f *TurnoverFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	period := f.Period
	if period <= 0 {
		period = 20 // 默认20日
	}

	// 计算起始日期 (往前推 period*2 天, 确保有足够的交易日数据)
	startDate := addDays(date, -period*2)

	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		basics, err := provider.GetDailyBasicByCode(tsCode, startDate, date)
		if err != nil {
			return nil, err
		}

		if len(basics) == 0 {
			// 数据不足, 跳过
			continue
		}

		// 取最近 period 个交易日的数据
		startIdx := len(basics) - period
		if startIdx < 0 {
			startIdx = 0
		}
		recent := basics[startIdx:]

		// 计算平均换手率
		var sum float64
		var count int
		for _, b := range recent {
			if b.TurnoverRate > 0 {
				sum += b.TurnoverRate
				count++
			}
		}

		if count == 0 {
			continue
		}

		avgTurnover := sum / float64(count)
		result[tsCode] = avgTurnover
	}

	return result, nil
}

// VolumeRatioFactor 量比因子 (5日均量 / 20日均量, 越高越好)
// 成交量放大说明有资金进场
// 方向: 正向 (量比大于1说明放量)
type VolumeRatioFactor struct {
	ShortPeriod int // 短期均量周期, 默认5日
	LongPeriod  int // 长期均量周期, 默认20日
}

// Name 返回因子名称
func (f *VolumeRatioFactor) Name() string {
	short := f.ShortPeriod
	if short <= 0 {
		short = 5
	}
	long := f.LongPeriod
	if long <= 0 {
		long = 20
	}
	return fmt.Sprintf("volume_ratio_%d_%d", short, long)
}

// Compute 计算量比因子值
// 短期均量 / 长期均量, 比值越大说明近期放量越明显, 越高越好
func (f *VolumeRatioFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	shortPeriod := f.ShortPeriod
	if shortPeriod <= 0 {
		shortPeriod = 5
	}
	longPeriod := f.LongPeriod
	if longPeriod <= 0 {
		longPeriod = 20
	}
	// 确保长期周期大于短期周期
	if longPeriod < shortPeriod {
		longPeriod = shortPeriod
	}

	// 计算起始日期 (往前推 longPeriod*2 天, 确保有足够的交易日数据)
	startDate := addDays(date, -longPeriod*2)

	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		bars, err := provider.GetBars(tsCode, startDate, date)
		if err != nil {
			return nil, err
		}

		if len(bars) < shortPeriod {
			// 数据不足, 跳过
			continue
		}

		// 计算短期均量 (最近 shortPeriod 日)
		shortStart := len(bars) - shortPeriod
		if shortStart < 0 {
			shortStart = 0
		}
		shortBars := bars[shortStart:]
		var shortSum float64
		for _, b := range shortBars {
			shortSum += b.Vol
		}
		shortAvg := shortSum / float64(len(shortBars))

		// 计算长期均量 (最近 longPeriod 日)
		longStart := len(bars) - longPeriod
		if longStart < 0 {
			longStart = 0
		}
		longBars := bars[longStart:]
		var longSum float64
		for _, b := range longBars {
			longSum += b.Vol
		}
		longAvg := longSum / float64(len(longBars))

		if longAvg <= 0 {
			continue
		}

		// 量比 = 短期均量 / 长期均量
		volumeRatio := shortAvg / longAvg
		result[tsCode] = volumeRatio
	}

	return result, nil
}

// LimitMotionFactor 涨跌停因子 (最近N日涨停次数 - 跌停次数, 越高越好)
// 用涨停次数/跌停次数衡量个股活跃度和强势程度
// 方向: 正向 (涨停多说明强势)
type LimitMotionFactor struct {
	Period int // 回看周期, 默认20日
}

// Name 返回因子名称
func (f *LimitMotionFactor) Name() string {
	return fmt.Sprintf("limit_motion_%d", f.Period)
}

// Compute 计算涨跌停因子值
// 最近 period 个交易日内 (涨停次数 - 跌停次数)
// 使用 limit_status 字段判断: 1涨停, -1跌停, 0正常
// 如果没有 limit_status 数据, 则用涨跌幅判断 (涨幅>=9.8%视为涨停, 跌幅<=-9.8%视为跌停)
func (f *LimitMotionFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	period := f.Period
	if period <= 0 {
		period = 20 // 默认20日
	}

	// 计算起始日期 (往前推 period*2 天, 确保有足够的交易日数据)
	startDate := addDays(date, -period*2)

	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		basics, err := provider.GetDailyBasicByCode(tsCode, startDate, date)
		if err != nil {
			return nil, err
		}

		if len(basics) == 0 {
			// 数据不足, 跳过
			continue
		}

		// 取最近 period 个交易日的数据
		startIdx := len(basics) - period
		if startIdx < 0 {
			startIdx = 0
		}
		recent := basics[startIdx:]

		// 统计涨停次数和跌停次数
		limitUpCount := 0
		limitDownCount := 0

		for _, b := range recent {
			if b.LimitStatus == 1 {
				limitUpCount++
			} else if b.LimitStatus == -1 {
				limitDownCount++
			}
		}

		// 因子值 = 涨停次数 - 跌停次数
		result[tsCode] = float64(limitUpCount - limitDownCount)
	}

	return result, nil
}
