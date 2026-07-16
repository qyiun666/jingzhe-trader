package signal

import (
	"sync"

	"jingzhe-trader/internal/factor"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// FactorDataProvider 因子数据提供者，封装存储层，实现 factor.DataProvider 接口
// 内置内存缓存，避免重复查询数据库
type FactorDataProvider struct {
	basicRepo *store.BasicRepo
	finaRepo  *store.FinaRepo
	stockRepo *store.StockRepo
	barRepo   *store.BarRepo

	// 缓存
	dailyBasicCache    map[string][]model.DailyBasic   // date -> basics
	dailyBasicCodeCache map[string][]model.DailyBasic  // tsCode:start:end -> basics
	finaCache          map[string][]model.FinaIndicator // tsCode -> indicators
	stockCache         map[string]*model.Stock          // tsCode -> stock
	barCache           map[string][]model.Bar           // tsCode:start:end -> bars

	mu sync.RWMutex
}

// NewFactorDataProvider 构造 FactorDataProvider
func NewFactorDataProvider(
	basicRepo *store.BasicRepo,
	finaRepo *store.FinaRepo,
	stockRepo *store.StockRepo,
	barRepo *store.BarRepo,
) *FactorDataProvider {
	return &FactorDataProvider{
		basicRepo:          basicRepo,
		finaRepo:           finaRepo,
		stockRepo:          stockRepo,
		barRepo:            barRepo,
		dailyBasicCache:    make(map[string][]model.DailyBasic),
		dailyBasicCodeCache: make(map[string][]model.DailyBasic),
		finaCache:          make(map[string][]model.FinaIndicator),
		stockCache:         make(map[string]*model.Stock),
		barCache:           make(map[string][]model.Bar),
	}
}

// GetDailyBasic 获取指定交易日的全市场基本面数据 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetDailyBasic(date string) ([]model.DailyBasic, error) {
	// 先查缓存
	p.mu.RLock()
	if basics, ok := p.dailyBasicCache[date]; ok {
		p.mu.RUnlock()
		return basics, nil
	}
	p.mu.RUnlock()

	// 缓存未命中，查询数据库
	basics, err := p.basicRepo.GetByDate(date)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	p.mu.Lock()
	p.dailyBasicCache[date] = basics
	p.mu.Unlock()

	return basics, nil
}

// GetDailyBasicByCode 获取指定股票在 [startDate, endDate] 区间内的基本面数据 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetDailyBasicByCode(tsCode, startDate, endDate string) ([]model.DailyBasic, error) {
	cacheKey := tsCode + ":" + startDate + ":" + endDate

	// 先查缓存
	p.mu.RLock()
	if basics, ok := p.dailyBasicCodeCache[cacheKey]; ok {
		p.mu.RUnlock()
		return basics, nil
	}
	p.mu.RUnlock()

	// 缓存未命中，查询数据库
	basics, err := p.basicRepo.GetByCode(tsCode, startDate, endDate)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	p.mu.Lock()
	p.dailyBasicCodeCache[cacheKey] = basics
	p.mu.Unlock()

	return basics, nil
}

// GetFinaIndicator 获取指定股票的全部财务指标 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetFinaIndicator(tsCode string) ([]model.FinaIndicator, error) {
	// 先查缓存
	p.mu.RLock()
	if fis, ok := p.finaCache[tsCode]; ok {
		p.mu.RUnlock()
		return fis, nil
	}
	p.mu.RUnlock()

	// 缓存未命中，查询数据库
	fis, err := p.finaRepo.GetByCode(tsCode)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	p.mu.Lock()
	p.finaCache[tsCode] = fis
	p.mu.Unlock()

	return fis, nil
}

// GetStockByCode 按代码查询股票基本信息 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetStockByCode(tsCode string) (*model.Stock, error) {
	// 先查缓存
	p.mu.RLock()
	if stock, ok := p.stockCache[tsCode]; ok {
		p.mu.RUnlock()
		return stock, nil
	}
	p.mu.RUnlock()

	// 缓存未命中，查询数据库
	stock, err := p.stockRepo.GetByCode(tsCode)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	p.mu.Lock()
	p.stockCache[tsCode] = stock
	p.mu.Unlock()

	return stock, nil
}

// GetBars 获取指定股票在 [startDate, endDate] 区间内的日线数据 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetBars(tsCode, startDate, endDate string) ([]model.Bar, error) {
	cacheKey := tsCode + ":" + startDate + ":" + endDate

	// 先查缓存
	p.mu.RLock()
	if bars, ok := p.barCache[cacheKey]; ok {
		p.mu.RUnlock()
		return bars, nil
	}
	p.mu.RUnlock()

	// 缓存未命中，查询数据库
	bars, err := p.barRepo.GetBars(tsCode, startDate, endDate)
	if err != nil {
		return nil, err
	}

	// 写入缓存
	p.mu.Lock()
	p.barCache[cacheKey] = bars
	p.mu.Unlock()

	return bars, nil
}

// ClearCache 清空所有缓存
func (p *FactorDataProvider) ClearCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dailyBasicCache = make(map[string][]model.DailyBasic)
	p.dailyBasicCodeCache = make(map[string][]model.DailyBasic)
	p.finaCache = make(map[string][]model.FinaIndicator)
	p.stockCache = make(map[string]*model.Stock)
	p.barCache = make(map[string][]model.Bar)
}

// 编译期接口检查
var _ factor.DataProvider = (*FactorDataProvider)(nil)
