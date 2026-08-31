package api

import (
	"fmt"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// ==================== 持仓同步 ====================

// SyncPortfolioRequest 持仓同步请求
type SyncPortfolioRequest struct {
	Positions []SyncPositionItem `json:"positions"` // 持仓列表
	Cash      float64            `json:"cash"`      // 可用现金（可选，默认从现有值推算）
	Overwrite *bool              `json:"overwrite"` // true=全量覆盖, false=逐条增量更新（缺省true）
}

// SyncPositionItem 单只持仓同步条目
type SyncPositionItem struct {
	TsCode       string  `json:"ts_code"`
	TotalQty     int     `json:"total_qty"`
	AvailableQty int     `json:"available_qty"`
	CostPrice    float64 `json:"cost_price"`
}

// SyncPortfolioResponse 持仓同步响应
type SyncPortfolioResponse struct {
	SyncedCount int      `json:"synced_count"`
	Positions   []string `json:"positions"` // 同步的股票代码列表
	TotalAsset  float64  `json:"total_asset"`
	Cash        float64  `json:"cash"`
}

// SyncPortfolio synchronizes the local portfolio with real broker positions.
func (s *Service) SyncPortfolio(req SyncPortfolioRequest) (*SyncPortfolioResponse, error) {
	if len(req.Positions) == 0 {
		return nil, fmt.Errorf("持仓列表不能为空")
	}

	// 1. 转换为 store 持仓格式
	storeItems := make([]store.PortfolioSyncItem, 0, len(req.Positions))
	var names []string

	for _, item := range req.Positions {
		if item.TsCode == "" || item.TotalQty <= 0 {
			continue
		}
		storeItems = append(storeItems, store.PortfolioSyncItem{
			TsCode:       item.TsCode,
			TotalQty:     item.TotalQty,
			AvailableQty: item.AvailableQty,
			CostPrice:    item.CostPrice,
		})
		names = append(names, s.stockName(item.TsCode))
	}
	positionMap := positionsToMap(storeItems)

	if len(storeItems) == 0 {
		return nil, fmt.Errorf("有效持仓为空")
	}

	// 2. 持久化到数据库: Overwrite=true 全量覆盖, false 逐条 Upsert
	overwrite := req.Overwrite == nil || *req.Overwrite
	portRepo := store.NewPortfolioRepo(s.db)
	if overwrite {
		if err := portRepo.SyncPortfolio(storeItems); err != nil {
			return nil, fmt.Errorf("持仓持久化失败: %w", err)
		}
	} else {
		for _, item := range storeItems {
			if err := portRepo.UpsertPosition(item); err != nil {
				return nil, fmt.Errorf("持仓增量更新失败: %w", err)
			}
		}
	}

	// 3. 更新内存中的 PaperBroker 持仓
	cash := req.Cash
	if cash <= 0 {
		// 从现有资产推算现金
		asset, _ := s.brk.QueryAsset()
		cash = asset.Cash
	}
	if !overwrite {
		// 增量模式: 内存重建为数据库全量持仓 (含本次未触及的旧持仓)
		// 失败必须中止: 留着空 map 会让下方本金核算把基准写成只记现金, 造成不可逆污染
		all, err := portRepo.GetAllPositions()
		if err != nil {
			return nil, fmt.Errorf("持仓增量更新后重建全量持仓失败: %w", err)
		}
		positionMap = positionsToMap(all)
	}
	s.importPositions(positionMap, cash)
	// 4. 记录现金到元数据; initial_capital 仅首次设置 (避免覆盖已有值导致总盈亏计算错误)
	// 首次同步本金 = 现金 + 持仓成本市值: 只记现金会把带入持仓的市值全部算成虚假盈利
	costValue := 0.0
	missingCost := false
	for _, p := range positionMap {
		if p.CostPrice <= 0 {
			missingCost = true
			continue
		}
		costValue += float64(p.TotalQty) * p.CostPrice
	}
	if err := portRepo.SetMeta("cash", fmt.Sprintf("%.2f", cash)); err != nil {
		logger.L().Warnf("[持仓同步] 写入 cash 失败, 重启后现金将回退到旧值: %v", err)
	}
	if existing, err := portRepo.GetMeta("initial_capital"); err != nil {
		// 真实查询错误时跳过写入: 盲写会覆盖/错设资金基准且仅首次写入不可自愈
		logger.L().Warnf("[持仓同步] 查询 initial_capital 失败, 跳过本金初始化: %v", err)
	} else if existing == "" && missingCost {
		// 成本价缺失时不写本金: write-once 基准宁缺勿错, 留空回退 config 并提醒补传 cost_price
		logger.L().Warnf("[持仓同步] 存在 cost_price 缺失的持仓, 跳过本金初始化, 请带完整成本价重新同步")
	} else if existing == "" {
		if err := portRepo.SetMeta("initial_capital", fmt.Sprintf("%.2f", cash+costValue)); err != nil {
			logger.L().Warnf("[持仓同步] 写入 initial_capital 失败: %v", err)
		}
	}

	return &SyncPortfolioResponse{
		SyncedCount: len(storeItems),
		Positions:   names,
		TotalAsset:  cash + costValue,
		Cash:        cash,
	}, nil
}

// positionsToMap 将数据库持仓列表转为 PaperBroker 导入用的 map (同步增量重建与启动恢复共用)
func positionsToMap(items []store.PortfolioSyncItem) map[string]*model.Position {
	m := make(map[string]*model.Position, len(items))
	for _, p := range items {
		m[p.TsCode] = &model.Position{
			TsCode:       p.TsCode,
			TotalQty:     p.TotalQty,
			AvailableQty: p.AvailableQty,
			TodayBought:  p.TodayBought,
			HighPrice:    p.HighPrice,
			CostPrice:    p.CostPrice,
		}
	}
	return m
}

// PositionDetail is a single portfolio holding with market pricing.
type PositionDetail struct {
	TsCode         string  `json:"ts_code"`
	Name           string  `json:"name"`
	TotalQty       int     `json:"total_qty"`
	AvailableQty   int     `json:"available_qty"`
	CostPrice      float64 `json:"cost_price"`
	AvgPrice       float64 `json:"avg_price"`
	MarketPrice    float64 `json:"market_price"`
	MarketValue    float64 `json:"market_value"`
	FloatingPnL    float64 `json:"floating_pnl"`
	FloatingPnLPct float64 `json:"floating_pnl_pct"`
}

// BuildPortfolio returns the current holdings from the database enriched with
// the latest market prices.
func (s *Service) BuildPortfolio() []PositionDetail {
	portRepo := store.NewPortfolioRepo(s.db) // PortfolioRepo 含元数据操作, 暂不提升为共享字段
	positions, err := portRepo.GetAllPositions()
	if err != nil {
		return []PositionDetail{}
	}

	// 最新行情: 按各标的自身最新一根日线估值
	// 不能只取"今天"的 bar —— 盘前与当日收盘数据到位之前今天是空的,
	// 那样每个持仓都会退化成按成本价估值, 浮盈恒为 0, 止损与回撤判断失去依据
	codes := make([]string, 0, len(positions))
	for _, p := range positions {
		codes = append(codes, p.TsCode)
	}
	latest, err := s.barRepo.GetLatestBars(codes)
	if err != nil {
		logger.L().Warnw("查询持仓最新行情失败, 退化为按成本价估值", "err", err)
	}
	barMap := make(map[string]float64, len(latest))
	for _, b := range latest {
		barMap[b.TsCode] = b.Close
	}

	var result []PositionDetail
	for _, p := range positions {
		detail := PositionDetail{
			TsCode:       p.TsCode,
			Name:         s.stockName(p.TsCode),
			TotalQty:     p.TotalQty,
			AvailableQty: p.AvailableQty,
			CostPrice:    p.CostPrice,
			AvgPrice:     p.CostPrice, // 与 cost_price 恒等, 保留键仅为响应兼容
		}
		price := 0.0
		if close, ok := barMap[p.TsCode]; ok && close > 0 {
			price = close
		} else if p.CostPrice > 0 {
			// 当日无行情(停牌/数据缺失)时用成本价兜底, 避免估值置0导致总资产/回撤误报
			price = p.CostPrice
		}
		if price > 0 {
			detail.MarketPrice = price
			detail.MarketValue = price * float64(p.TotalQty)
			if p.CostPrice > 0 {
				detail.FloatingPnL = detail.MarketValue - p.CostPrice*float64(p.TotalQty)
				detail.FloatingPnLPct = detail.FloatingPnL / (p.CostPrice * float64(p.TotalQty))
			}
		}
		result = append(result, detail)
	}

	return result
}
