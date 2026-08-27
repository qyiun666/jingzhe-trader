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
	TsCode string  `json:"ts_code"` // 股票代码
	Side   string  `json:"side"`    // "buy" 或 "sell"
	Qty    int     `json:"qty"`     // 成交数量
	Price  float64 `json:"price"`   // 成交价格
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

	asset := s.applyTradeToPortfolio(req.TsCode, side, req.Qty, req.Price)

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
		return err
	}
	logger.L().Infof("[T+1结算] %s 持仓结转完成", date)
	return nil
}

// applyTradeToPortfolio 将成交同步到内存持仓与数据库 (trade/confirm 与 plan/confirm 共用)
func (s *Service) applyTradeToPortfolio(tsCode string, side model.Side, qty int, price float64) *broker.AssetInfo {
	// 1. 更新 PaperBroker 内存持仓
	if pb, ok := s.brk.(*broker.PaperBroker); ok {
		pb.RecordTrade(tsCode, side, qty, price)
	}

	// 2. 更新数据库持仓
	portRepo := store.NewPortfolioRepo(s.db)
	pos, _ := portRepo.GetPosition(tsCode)
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
		portRepo.UpsertPosition(store.PortfolioSyncItem{
			TsCode:       tsCode,
			TotalQty:     newQty,
			AvailableQty: pos.AvailableQty, // T+1: 今日买入明日可卖
			TodayBought:  pos.TodayBought + qty,
			HighPrice:    highPrice,
			CostPrice:    newCost,
			AvgPrice:     newCost,
		})
	} else {
		// 卖出: 减少持仓与可卖量, 清仓则删除记录
		newQty := pos.TotalQty - qty
		newAvail := pos.AvailableQty - qty
		if newAvail < 0 {
			newAvail = 0
		}
		if newQty <= 0 {
			portRepo.RemovePosition(tsCode)
		} else {
			portRepo.UpsertPosition(store.PortfolioSyncItem{
				TsCode:       tsCode,
				TotalQty:     newQty,
				AvailableQty: newAvail,
				TodayBought:  pos.TodayBought,
				HighPrice:    pos.HighPrice,
				CostPrice:    pos.CostPrice,
				AvgPrice:     pos.AvgPrice,
			})
		}
	}

	// 3. 成交落库 (绩效归因/收益曲线的数据源, 与回测 trades 同表区分 run_id)
	if _, err := store.NewTradeRepo(s.db).InsertTrade(&model.Trade{
		RunID:     liveSnapshotRunID,
		TsCode:    tsCode,
		Side:      side,
		Price:     price,
		Qty:       qty,
		Amount:    price * float64(qty),
		TradeDate: time.Now().Format("20060102"),
		TradeTime: time.Now().Format("20060102 150405"),
	}); err != nil {
		logger.L().Warnw("成交落库失败", "ts_code", tsCode, "err", err)
	}

	// 4. 查询更新后的资产并持久化 cash
	asset, _ := s.brk.QueryAsset()
	if asset != nil {
		portRepo.SetMeta("cash", fmt.Sprintf("%.2f", asset.Cash))
	} else {
		asset = &broker.AssetInfo{}
	}
	return asset
}
