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
	dailyBasicCache     map[string][]model.DailyBasic    // date -> basics
	dailyBasicCodeCache map[string][]model.DailyBasic    // tsCode:start:end -> basics
	finaCache           map[string][]model.FinaIndicator // tsCode -> indicators
	stockCache          map[string]*model.Stock          // tsCode -> stock
	barCache            map[string][]model.Bar           // tsCode:start:end -> bars

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
		basicRepo:           basicRepo,
		finaRepo:            finaRepo,
		stockRepo:           stockRepo,
		barRepo:             barRepo,
		dailyBasicCache:     make(map[string][]model.DailyBasic),
		dailyBasicCodeCache: make(map[string][]model.DailyBasic),
		finaCache:           make(map[string][]model.FinaIndicator),
		stockCache:          make(map[string]*model.Stock),
		barCache:            make(map[string][]model.Bar),
	}
}

// cached 通用缓存访问: RLock 查缓存 → 未命中调 load → Lock 写缓存
// 5 个数据访问方法共用同一模板
func cached[T any](p *FactorDataProvider, cache map[string]T, key string, load func() (T, error)) (T, error) {
	p.mu.RLock()
	if v, ok := cache[key]; ok {
		p.mu.RUnlock()
		return v, nil
	}
	p.mu.RUnlock()

	v, err := load()
	if err != nil {
		return v, err
	}

	p.mu.Lock()
	cache[key] = v
	p.mu.Unlock()
	return v, nil
}

// GetDailyBasic 获取指定交易日的全市场基本面数据 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetDailyBasic(date string) ([]model.DailyBasic, error) {
	return cached(p, p.dailyBasicCache, date, func() ([]model.DailyBasic, error) {
		return p.basicRepo.GetByDate(date)
	})
}

// GetDailyBasicByCode 获取指定股票在 [startDate, endDate] 区间内的基本面数据 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetDailyBasicByCode(tsCode, startDate, endDate string) ([]model.DailyBasic, error) {
	key := tsCode + ":" + startDate + ":" + endDate
	return cached(p, p.dailyBasicCodeCache, key, func() ([]model.DailyBasic, error) {
		return p.basicRepo.GetByCode(tsCode, startDate, endDate)
	})
}

// GetFinaIndicator 获取指定股票的全部财务指标 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetFinaIndicator(tsCode string) ([]model.FinaIndicator, error) {
	return cached(p, p.finaCache, tsCode, func() ([]model.FinaIndicator, error) {
		return p.finaRepo.GetByCode(tsCode)
	})
}

// GetStockByCode 按代码查询股票基本信息 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetStockByCode(tsCode string) (*model.Stock, error) {
	return cached(p, p.stockCache, tsCode, func() (*model.Stock, error) {
		return p.stockRepo.GetByCode(tsCode)
	})
}

// GetBars 获取指定股票在 [startDate, endDate] 区间内的日线数据 (实现 factor.DataProvider 接口)
func (p *FactorDataProvider) GetBars(tsCode, startDate, endDate string) ([]model.Bar, error) {
	key := tsCode + ":" + startDate + ":" + endDate
	return cached(p, p.barCache, key, func() ([]model.Bar, error) {
		return p.barRepo.GetBars(tsCode, startDate, endDate)
	})
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
