package api

import (
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// liveSnapshotRunID 实盘账户快照的 run_id (与回测 bt_* 区分)
const liveSnapshotRunID = store.RunIDLive

// ==================== 交易反馈 ====================

// TradeConfirmRequest 交易确认请求
type TradeConfirmRequest struct {
	TsCode string  `json:"ts_code"`           // 股票代码
	Side   string  `json:"side"`              // "buy" 或 "sell"
	Qty    int     `json:"qty"`               // 成交数量
	Price  float64 `json:"price"`             // 成交价格
	PlanID int64   `json:"plan_id,omitempty"` // 关联的交易计划ID (人工确认后删除该计划)
}

// TradeConfirmResponse 交易确认响应
type TradeConfirmResponse struct {
	TsCode     string  `json:"ts_code"`
	Name       string  `json:"name"`
	Side       string  `json:"side"`
	Qty        int     `json:"qty"`
	Price      float64 `json:"price"`
	Amount     float64 `json:"amount"`
	Cash       float64 `json:"cash"`        // 更新后现金
	TotalAsset float64 `json:"total_asset"` // 更新后总资产
}

// ConfirmTrade validates the request, applies the trade to the portfolio,
// persists it, and returns the trade confirmation response.
func (s *Service) ConfirmTrade(req TradeConfirmRequest) (*TradeConfirmResponse, error) {
	// 参数校验
	req.TsCode = strings.TrimSpace(req.TsCode)
	if req.TsCode == "" {
		return nil, fmt.Errorf("ts_code 不能为空")
	}
	req.Side = strings.ToLower(strings.TrimSpace(req.Side))
	if req.Side != "buy" && req.Side != "sell" {
		return nil, fmt.Errorf("side 必须为 buy 或 sell")
	}
	if req.Qty <= 0 || req.Qty%100 != 0 {
		return nil, fmt.Errorf("qty 必须是100的整数倍")
	}
	if req.Price <= 0 {
		return nil, fmt.Errorf("price 必须大于0")
	}

	// 确定买卖方向
	side := model.SideBuy
	if req.Side == "sell" {
		side = model.SideSell
	}

	asset, err := s.applyTradeToPortfolio(req.TsCode, side, req.Qty, req.Price)
	if err != nil {
		return nil, fmt.Errorf("成交同步持仓失败: %w", err)
	}

	// 人工成交完成后删除关联计划 (成交审计在 action_log, 计划不再保留)
	if req.PlanID > 0 {
		if err := s.closePlan(req.PlanID, req.TsCode, req.Side); err != nil {
			logger.L().Warnw("关闭交易计划失败", "plan_id", req.PlanID, "err", err)
		}
	}

	return &TradeConfirmResponse{
		TsCode:     req.TsCode,
		Name:       s.stockName(req.TsCode),
		Side:       req.Side,
		Qty:        req.Qty,
		Price:      req.Price,
		Amount:     req.Price * float64(req.Qty),
		Cash:       asset.Cash,
		TotalAsset: asset.TotalAsset,
	}, nil
}

// closePlan 人工成交后关闭关联计划: 校验代码/方向一致后删除 (成交审计在 action_log)
func (s *Service) closePlan(planID int64, tsCode, direction string) error {
	plan, err := s.planRepo.GetPlanByID(planID)
	if err != nil {
		return err
	}
	if plan.TsCode != tsCode || strings.ToLower(plan.Direction) != direction {
		return fmt.Errorf("计划(%s %s)与成交(%s %s)不一致", plan.TsCode, plan.Direction, tsCode, direction)
	}
	return s.planRepo.DeletePlan(planID)
}

// PollBrokerTrades 轮询券商端成交回报 (QMT 模式由对账任务调用, paper 模式无操作)
func (s *Service) PollBrokerTrades() error {
	if qb, ok := s.brk.(*broker.QMTBridge); ok {
		return qb.PollTrades()
	}
	return nil
}

// SettleT1 每日开盘前结转: 内存 broker 与 DB 持仓的 T+1 交收 (昨日买入转为可卖)
func (s *Service) SettleT1(date string) error {
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.SettleT1(date)
	}
	if err := store.NewPortfolioRepo(s.db).SettleT1(); err != nil {
		return fmt.Errorf("T+1持仓结转失败: %w", err)
	}
	logger.L().Infof("[T+1结算] %s 持仓结转完成", date)
	return nil
}

// applyTradeToPortfolio 将成交同步到内存持仓与数据库 (trade/confirm 与 plan/confirm 共用)
// 持仓写库失败必须上抛: 内存已成交而 DB 未更新时, 重启后会以 DB 为准恢复, 成交将静默丢失
func (s *Service) applyTradeToPortfolio(tsCode string, side model.Side, qty int, price float64) (*broker.AssetInfo, error) {
	// 1. 更新 PaperBroker 内存持仓
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.RecordTrade(tsCode, side, qty, price)
	}

	// 2. 更新数据库持仓
	portRepo := store.NewPortfolioRepo(s.db)
	pos, err := portRepo.GetPosition(tsCode)
	if err != nil {
		return nil, fmt.Errorf("查询持仓失败 %s: %w", tsCode, err)
	}
	if pos == nil {
		pos = &store.PortfolioSyncItem{} // 买入新股票时 pos 为 nil
	}

	if side == model.SideBuy {
		// 买入: 更新或新增持仓, 加权平均成本
		// T+1: 今日买入计入 today_bought, 不计入可卖量, 次日开盘结转后可卖
		newQty := pos.TotalQty + qty
		newCost := pos.CostPrice
		if newQty > 0 && pos.TotalQty > 0 {
			oldTotal := pos.CostPrice * float64(pos.TotalQty)
			newCost = (oldTotal + price*float64(qty)) / float64(newQty)
		} else if pos.TotalQty == 0 {
			newCost = price
		}
		highPrice := pos.HighPrice
		if price > highPrice {
			highPrice = price // 买入价也可能是持仓期新高
		}
		if err := portRepo.UpsertPosition(store.PortfolioSyncItem{
			TsCode:       tsCode,
			TotalQty:     newQty,
			AvailableQty: pos.AvailableQty, // T+1: 今日买入明日可卖
			TodayBought:  pos.TodayBought + qty,
			HighPrice:    highPrice,
			CostPrice:    newCost,
		}); err != nil {
			return nil, fmt.Errorf("买入更新持仓失败 %s: %w", tsCode, err)
		}
	} else {
		// 卖出: 减少持仓与可卖量, 清仓则删除记录
		newQty := pos.TotalQty - qty
		newAvail := pos.AvailableQty - qty
		if newAvail < 0 {
			newAvail = 0
		}
		if newQty <= 0 {
			if err := portRepo.RemovePosition(tsCode); err != nil {
				return nil, fmt.Errorf("清仓删除持仓失败 %s: %w", tsCode, err)
			}
		} else if err := portRepo.UpsertPosition(store.PortfolioSyncItem{
			TsCode:       tsCode,
			TotalQty:     newQty,
			AvailableQty: newAvail,
			TodayBought:  pos.TodayBought,
			HighPrice:    pos.HighPrice,
			CostPrice:    pos.CostPrice,
		}); err != nil {
			return nil, fmt.Errorf("卖出更新持仓失败 %s: %w", tsCode, err)
		}
	}

	// 3. 人工成交流水落库到 action_log (kind=trade; 人工成交的唯一数据源, 不再写 trades 表)
	asset, _ := s.brk.QueryAsset()
	var cash, totalAsset float64
	if asset != nil {
		cash, totalAsset = asset.Cash, asset.TotalAsset
	}
	if err := s.actionRepo.AddTrade(time.Now().Format("20060102"), store.TradeFill{
		TsCode:     tsCode,
		Side:       side.String(),
		Qty:        qty,
		Price:      price,
		Amount:     price * float64(qty),
		Cash:       cash,
		TotalAsset: totalAsset,
	}); err != nil {
		logger.L().Warnw("人工成交流水记录失败", "ts_code", tsCode, "err", err)
	}

	// 4. 持久化 cash (重启后以 DB 为准)
	if asset != nil {
		if err := portRepo.SetMeta("cash", fmt.Sprintf("%.2f", asset.Cash)); err != nil {
			logger.L().Warnw("成交后持久化 cash 失败, 重启后现金将回退到旧值", "ts_code", tsCode, "err", err)
		}
	} else {
		asset = &broker.AssetInfo{}
	}
	return asset, nil
}
