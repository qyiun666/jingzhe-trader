package api

import (
	"errors"
	"fmt"
	"strconv"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// ==================== 目标跟踪 ====================

// goalAdjustedRisk 返回按季度目标状态调节后的风控配置 (只收紧不放松)
// 第二个返回值: 调整说明 (空=未调整或未启用)
func (s *Service) goalAdjustedRisk(date string) (config.RiskConfig, []string) {
	if s.goalTracker == nil {
		return s.cfg.Risk, nil
	}
	asset := s.getAsset()
	st, err := s.goalTracker.Status(date, asset.TotalAsset)
	if err != nil {
		logger.L().Warnf("[目标跟踪] 状态计算失败, 使用基础风控: %v", err)
		return s.cfg.Risk, nil
	}
	adj, notes := s.goalTracker.AdjustRisk(s.cfg.Risk, st)
	for _, n := range notes {
		logger.L().Infof("[目标跟踪] %s %s", date, n)
	}
	return adj, notes
}

// GoalStatus 返回当前季度目标状态 (供 API 与调度器使用)
func (s *Service) GoalStatus(date string) (*goal.Status, error) {
	if s.goalTracker == nil {
		return nil, fmt.Errorf("目标跟踪未启用 (goal.enabled=false)")
	}
	asset := s.getAsset()
	return s.goalTracker.Status(date, asset.TotalAsset)
}

// RecordLiveSnapshot 记录实盘每日账户快照 (收益曲线数据, 供日报/目标跟踪/复盘使用)
// 数据更新成功后调用: 用当日收盘价更新市值 → 计算当日/累计盈亏 → 落 account_snapshot
func (s *Service) RecordLiveSnapshot(date string) error {
	bars, err := s.barRepo.GetBarsByDate(date)
	if err != nil {
		return fmt.Errorf("查询当日行情失败: %w", err)
	}
	if len(bars) == 0 {
		return errors.New("无当日行情, 跳过快照")
	}
	barMap := barsToMap(bars)
	s.brk.UpdateMarketValue(barMap)
	asset, err := s.brk.QueryAsset()
	if err != nil {
		return fmt.Errorf("查询资产失败: %w", err)
	}

	snap := model.AccountSnapshot{
		TradeDate:   date,
		TotalAsset:  asset.TotalAsset,
		Cash:        asset.Cash,
		MarketValue: asset.MarketValue,
	}
	tradeRepo := store.NewTradeRepo(s.db)
	// 当日盈亏: 对比上一交易日快照 (不能用当日已有记录作基准, 否则同日补记时盈亏被归零)
	// 查询错误必须中止: 同日补记走 INSERT OR REPLACE, 吞错会用 pnl=0 覆盖已写对的正确记录
	prev, err := tradeRepo.GetAccountSnapshotBefore(liveSnapshotRunID, date)
	if err != nil {
		return fmt.Errorf("查询上一交易日快照失败: %w", err)
	}
	// 累计盈亏基准: 错误同样中止, 避免用 config 回退值覆盖已写对的累计值
	initial, err := s.liveInitialCapital()
	if err != nil {
		return err
	}
	snap.FillPnL(prev, initial)
	if err := tradeRepo.InsertAccountSnapshot(liveSnapshotRunID, snap); err != nil {
		return fmt.Errorf("快照落库失败: %w", err)
	}
	logger.L().Infof("[实盘快照] %s 总资产 %.2f 当日盈亏 %.2f (%.2f%%) 累计 %.2f%%",
		date, snap.TotalAsset, snap.PnL, snap.PnLPct*100, snap.TotalPnLPct*100)
	return nil
}

// liveInitialCapital 实盘累计盈亏基准: 优先 config_kv.initial_capital,
// 无记录或非正数时回退配置的回测初始资金; 仅 meta 查询失败返回 error
func (s *Service) liveInitialCapital() (float64, error) {
	v, err := store.NewPortfolioRepo(s.db).GetMeta("initial_capital")
	if err != nil {
		return 0, fmt.Errorf("查询 initial_capital 失败: %w", err)
	}
	if f, perr := strconv.ParseFloat(v, 64); perr == nil && f > 0 {
		return f, nil
	}
	return s.cfg.Backtest.InitialCapital, nil
}
