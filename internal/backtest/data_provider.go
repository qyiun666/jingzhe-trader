package backtest

import (
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// DataProvider 回测数据提供者
// 预加载股票池历史数据到内存, 实现 strategy.HistoryProvider 接口
// 价格均为前复权 (AdjustBarsForward), 原始因子比值缓存在 adjRatio 供涨跌停口径换算
type DataProvider struct {
	// barsByCode: 每只股票的全部日线 (前复权后), 按日期升序
	barsByCode map[string][]model.Bar
	// dateIndex: 每只股票 日期->在barsByCode中的索引
	dateIndex map[string]map[string]int
	// adjRatio: 每只股票 日期->复权因子比值 (adj_date/adj_latest, AdjustBarsForward 前缓存)
	adjRatio map[string]map[string]float64
}

// NewDataProvider 从数据库预加载股票池数据
func NewDataProvider(barRepo *store.BarRepo, universe []string, startDate, endDate string) (*DataProvider, error) {
	dp := &DataProvider{
		barsByCode: make(map[string][]model.Bar),
		dateIndex:  make(map[string]map[string]int),
		adjRatio:   make(map[string]map[string]float64),
	}

	for _, tsCode := range universe {
		bars, err := barRepo.GetBars(tsCode, startDate, endDate)
		if err != nil {
			return nil, err
		}
		if len(bars) == 0 {
			continue
		}
		// 前复权前缓存原始因子比值 (复权后因子归一化, 涨跌停换算需原始比值)
		dp.adjRatio[tsCode] = adjRatiosOf(bars)
		// 前复权 (以最后一天有效因子为基准, 成交量反向调整; 因子缺失沿用上日)
		model.AdjustBarsForward(bars)

		idxMap := make(map[string]int, len(bars))
		for i, b := range bars {
			idxMap[b.TradeDate] = i
		}
		dp.barsByCode[tsCode] = bars
		dp.dateIndex[tsCode] = idxMap
	}

	return dp, nil
}

// adjRatiosOf 计算每根K线复权因子相对最新因子的比值 (adj_i/adj_last)
// 与 AdjustBarsForward 同一基准逻辑, 必须在复权前调用
func adjRatiosOf(bars []model.Bar) map[string]float64 {
	lastAdj := 0.0
	for i := len(bars) - 1; i >= 0; i-- {
		if bars[i].AdjFactor > 0 {
			lastAdj = bars[i].AdjFactor
			break
		}
	}
	if lastAdj <= 0 {
		return nil
	}
	ratios := make(map[string]float64, len(bars))
	lastValid := 0.0
	for i := range bars {
		adj := bars[i].AdjFactor
		if adj <= 0 {
			adj = lastValid
		} else {
			lastValid = adj
		}
		if adj <= 0 {
			continue
		}
		ratios[bars[i].TradeDate] = adj / lastAdj
	}
	return ratios
}

// HasData 判断指定股票是否有已加载的行情数据
func (dp *DataProvider) HasData(tsCode string) bool {
	bars, ok := dp.barsByCode[tsCode]
	return ok && len(bars) > 0
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

// AdjRatio 返回指定日期复权因子相对最新因子的比值 (adj_date/adj_latest)
// 用于将前复权价换算回原始价 (如涨跌停价比较), 无数据时返回 1
func (dp *DataProvider) AdjRatio(tsCode, date string) float64 {
	if m, ok := dp.adjRatio[tsCode]; ok {
		if r, ok := m[date]; ok {
			return r
		}
	}
	return 1
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
