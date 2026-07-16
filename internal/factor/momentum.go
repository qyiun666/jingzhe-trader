package factor

import (
	"context"
	"fmt"
	"time"
)

// MomentumFactor 价格动量因子 (过去N日涨幅, 越高越好)
type MomentumFactor struct {
	Period int // 回看周期, 默认60日
}

// Name 返回因子名称
func (f *MomentumFactor) Name() string {
	return fmt.Sprintf("momentum_%d", f.Period)
}

// Compute 计算动量因子值
// 计算过去period日的涨跌幅, 越高越好
// 使用前复权收盘价计算涨跌幅
func (f *MomentumFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error) {
	period := f.Period
	if period <= 0 {
		period = 60 // 默认60日
	}

	// 计算起始日期 (往前推 period * 2 天, 确保有足够的交易日数据)
	startDate := addDays(date, -period*2)

	result := make(map[string]float64, len(universe))

	for _, tsCode := range universe {
		bars, err := provider.GetBars(tsCode, startDate, date)
		if err != nil {
			return nil, err
		}

		if len(bars) < 2 {
			// 数据不足, 跳过
			continue
		}

		// 找到截止日期前最近的一根K线 (通常就是date当天)
		lastBar := bars[len(bars)-1]

		// 找到 period 个交易日前的K线
		// 从最后一根往前数 period 根
		startIdx := len(bars) - 1 - period
		if startIdx < 0 {
			startIdx = 0
		}
		startBar := bars[startIdx]

		// 计算涨跌幅 (使用前复权价格)
		startPrice := startBar.AdjClose()
		endPrice := lastBar.AdjClose()

		if startPrice <= 0 {
			continue
		}

		// 涨跌幅百分比
		momentum := (endPrice - startPrice) / startPrice * 100
		result[tsCode] = momentum
	}

	return result, nil
}

// addDays 给日期字符串加减天数
// date 格式: YYYYMMDD
func addDays(date string, days int) string {
	t, err := time.Parse("20060102", date)
	if err != nil {
		return date
	}
	t = t.AddDate(0, 0, days)
	return t.Format("20060102")
}
