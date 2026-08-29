package api

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/strategy"
	"jingzhe-trader/pkg/logger"
)

// ==================== 系统维护 ====================

// SystemStatus 系统状态
type SystemStatus struct {
	Healthy        bool   `json:"healthy"`
	LastDataDate   string `json:"last_data_date"` // 数据库中最新的行情日期
	Today          string `json:"today"`
	DataFresh      bool   `json:"data_fresh"` // 数据是否是最新的
	Uptime         string `json:"uptime"`
	PortfolioCount int    `json:"portfolio_count"`  // 持仓数量
	NextMarketOpen string `json:"next_market_open"` // 下一个交易日
}

// BuildSystemStatus returns overall system status.
func (s *Service) BuildSystemStatus() SystemStatus {
	status := SystemStatus{
		Healthy: true,
		Today:   time.Now().Format("20060102"),
		Uptime:  time.Since(s.startTime).Truncate(time.Second).String(),
	}
	if err := s.db.Ping(); err != nil {
		status.Healthy = false
		return status
	}
	if maxDate, err := s.barRepo.GetMaxTradeDate(); err == nil {
		status.LastDataDate = maxDate
		if preDate, perr := s.calRepo.GetPreTradeDate(status.Today); perr == nil && preDate != "" {
			status.DataFresh = maxDate >= preDate
		}
	}
	if positions, err := store.NewPortfolioRepo(s.db).GetAllPositions(); err == nil { // 同上
		status.PortfolioCount = len(positions)
	}
	if nextDate, err := s.calRepo.GetNextTradeDate(status.Today); err == nil {
		status.NextMarketOpen = nextDate
	}
	return status
}

// UpdateData 进程内执行增量数据更新 (从库内最新日期补到今天); 同一时刻只允许一个更新任务
// 非阻塞: 如果有其他更新在执行, 立即返回错误
func (s *Service) UpdateData() error {
	if !s.updateMu.TryLock() {
		return fmt.Errorf("数据更新任务正在执行中, 请稍后重试")
	}
	defer s.updateMu.Unlock()
	return s.doUpdateData()
}

// UpdateDataBlocking 阻塞版数据更新: 等待其他更新完成后执行 (供信号任务前置调用)
func (s *Service) UpdateDataBlocking() error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	return s.doUpdateData()
}

// doUpdateData 增量数据更新核心逻辑
func (s *Service) doUpdateData() error {
	opts := s.buildUpdateOptions()
	return dataloader.New(s.cfg, s.db).Run(opts)
}

// buildUpdateOptions 构造每日增量更新的同步选项:
// 核心数据 (日线/基本面/涨跌停) 总是同步; 可选数据 (新股/新闻/资金流/龙虎榜/财务指标)
// 由 dataloader.sync_optional 控制 (默认开)。可选数据是辩论 Agent 与选股因子的输入,
// 不同步则新闻分析师/资金面上下文空转 (2026-08 审计发现三表均 0 行的根因)
func (s *Service) buildUpdateOptions() dataloader.Options {
	opts := dataloader.Options{}
	if maxDate, err := s.barRepo.GetMaxTradeDate(); err == nil && maxDate != "" {
		opts.StartDate = maxDate // 增量: 从库内最新日期补起 (含当日, 幂等覆盖)
	}
	if s.cfg.Dataloader.SyncOptional {
		opts.SyncNewShare = true
		opts.SyncNews = true
		opts.SyncMoneyFlow = true
		opts.SyncTopList = true
		opts.SyncFina = true
	}
	return opts
}

// SyncCalendar 仅同步交易日历 (轻量级, 供调度器打破日历死锁)
// 同步近一周到后一周的日历数据, 确保今天和近期日期都在日历中
func (s *Service) SyncCalendar() error {
	start := time.Now().AddDate(0, 0, -7).Format("20060102")
	end := time.Now().AddDate(0, 0, 7).Format("20060102")
	return dataloader.New(s.cfg, s.db).SyncCalendarOnly(start, end)
}

// ==================== 扩展初始化 ====================

// initExtensions 初始化扩展功能（在 NewService 中调用）
func (s *Service) initExtensions() {
	// 初始化动态策略选择器 (advisor 带真实指数收益序列)
	reg := strategy.DefaultRegistry()
	s.dynamicSelector = strategy.NewDynamicSelector(reg, &advisorAdapter{
		barRepo:   s.barRepo,
		tradeRepo: store.NewTradeRepo(s.db),
	})

	// 预热策略缓存: 为每个已注册策略创建并初始化实例, 避免运行时重建丢失内部状态
	for _, name := range reg.Names() {
		strat, ok := reg.Get(name)
		if !ok {
			continue
		}
		if err := strat.Init(context.Background(), s.cfg.StrategyParams(name)); err != nil {
			continue
		}
		s.strategyCache[name] = strat
	}

	// 尝试从数据库恢复持仓到内存
	s.restorePortfolioFromDB()
}

// restorePortfolioFromDB 从数据库恢复持仓到 PaperBroker
func (s *Service) restorePortfolioFromDB() {
	portRepo := store.NewPortfolioRepo(s.db)
	positions, err := portRepo.GetAllPositions()
	if err != nil || len(positions) == 0 {
		return // 无持仓数据，使用默认空仓
	}

	positionMap := positionsToMap(positions)

	// 优先读取实际 cash，其次 initial_capital，最后 fallback 到 config
	cash := s.cfg.Backtest.InitialCapital
	if cashStr, _ := portRepo.GetMeta("cash"); cashStr != "" {
		if v, err := strconv.ParseFloat(cashStr, 64); err == nil && v > 0 {
			cash = v
		}
	} else if capitalStr, _ := portRepo.GetMeta("initial_capital"); capitalStr != "" {
		if v, err := strconv.ParseFloat(capitalStr, 64); err == nil && v > 0 {
			cash = v
		}
	}

	// 导入到 PaperBroker
	s.importPositions(positionMap, cash)
}

// importPositions 导入持仓到 PaperBroker 并用库内最新收盘价现算市值
// 市值是派生值, 不随恢复/同步携带, 导入后一律现算
func (s *Service) importPositions(positions map[string]*model.Position, cash float64) {
	pb, ok := s.brk.(*broker.PaperBroker)
	if !ok {
		return
	}
	pb.ImportPositions(positions, cash)
	s.refreshMarketValueFromDB(pb)
}

// refreshMarketValueFromDB 用库内每只持仓最新一根日线收盘价现算市值 (停牌股取停牌前收盘)
func (s *Service) refreshMarketValueFromDB(pb *broker.PaperBroker) {
	pos := pb.GetPositions()
	codes := make([]string, 0, len(pos))
	for code := range pos {
		codes = append(codes, code)
	}
	bars, err := s.barRepo.GetLatestBars(codes)
	if err != nil {
		logger.L().Warnf("[持仓导入] 读取最新收盘价失败, 市值暂为0, 由 15:10 快照补正: %v", err)
		return
	}
	if len(bars) == 0 {
		logger.L().Warnw("[持仓导入] 库内无持仓股票行情, 市值暂为0, 由 15:10 快照补正")
		return
	}
	barMap := barsToMap(bars)
	pb.UpdateMarketValue(barMap)
}
