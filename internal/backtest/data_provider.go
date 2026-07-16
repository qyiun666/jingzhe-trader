package backtest

import (
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// DataProvider 回测数据提供者
// 预加载股票池历史数据到内存, 实现 strategy.HistoryProvider 接口
type DataProvider struct {
	// barsByCode: 每只股票的全部日线 (前复权后), 按日期升序
	barsByCode map[string][]model.Bar
	// dateIndex: 每只股票 日期->在barsByCode中的索引
	dateIndex map[string]map[string]int
}

// NewDataProvider 从数据库预加载股票池数据
func NewDataProvider(barRepo *store.BarRepo, universe []string, startDate, endDate string) (*DataProvider, error) {
	dp := &DataProvider{
		barsByCode: make(map[string][]model.Bar),
		dateIndex:   make(map[string]map[string]int),
	}

	for _, tsCode := range universe {
		bars, err := barRepo.GetBars(tsCode, startDate, endDate)
		if err != nil {
			return nil, err
		}
		if len(bars) == 0 {
			continue
		}
		// 计算前复权价 (使用最后一天的adj_factor作为基准)
		if len(bars) > 0 && bars[len(bars)-1].AdjFactor > 0 {
			lastAdj := bars[len(bars)-1].AdjFactor
			for i := range bars {
				if bars[i].AdjFactor > 0 {
					ratio := bars[i].AdjFactor / lastAdj
					bars[i].Open *= ratio
					bars[i].High *= ratio
					bars[i].Low *= ratio
					bars[i].Close *= ratio
					bars[i].PreClose *= ratio
				}
			}
		}

		idxMap := make(map[string]int, len(bars))
		for i, b := range bars {
			idxMap[b.TradeDate] = i
		}
		dp.barsByCode[tsCode] = bars
		dp.dateIndex[tsCode] = idxMap
	}

	return dp, nil
}

// GetBars 获取指定股票截至 endDate 的 N 根日线 (含 endDate)
func (dp *DataProvider) GetBars(tsCode, endDate string, n int) ([]model.Bar, error) {
	bars, ok := dp.barsByCode[tsCode]
	if !ok {
		return nil, nil
	}
	idxMap := dp.dateIndex[tsCode]
	endIdx, ok := idxMap[endDate]
	if !ok {
		return nil, nil
	}
	start := endIdx - n + 1
	if start < 0 {
		start = 0
	}
	return bars[start : endIdx+1], nil
}

// GetCloses 获取指定股票截至 endDate 的 N 个前复权收盘价
func (dp *DataProvider) GetCloses(tsCode, endDate string, n int) ([]float64, error) {
	bars, err := dp.GetBars(tsCode, endDate, n)
	if err != nil {
		return nil, err
	}
	closes := make([]float64, len(bars))
	for i, b := range bars {
		closes[i] = b.Close
	}
	return closes, nil
}

// GetBar 获取指定股票某日的K线
func (dp *DataProvider) GetBar(tsCode, date string) *model.Bar {
	bars, ok := dp.barsByCode[tsCode]
	if !ok {
		return nil
	}
	idxMap := dp.dateIndex[tsCode]
	idx, ok := idxMap[date]
	if !ok {
		return nil
	}
	return &bars[idx]
}

// GetNextBar 获取指定股票某日的下一日K线 (用于次日开盘价成交)
func (dp *DataProvider) GetNextBar(tsCode, date string) *model.Bar {
	bars, ok := dp.barsByCode[tsCode]
	if !ok {
		return nil
	}
	idxMap := dp.dateIndex[tsCode]
	idx, ok := idxMap[date]
	if !ok || idx+1 >= len(bars) {
		return nil
	}
	return &bars[idx+1]
}
