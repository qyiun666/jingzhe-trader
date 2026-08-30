package api

import (
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// GetKline returns K-line bars for a stock in a date range.
func (s *Service) GetKline(code, start, end string) ([]model.Bar, error) {
	if code == "" {
		return nil, fmt.Errorf("code 参数不能为空")
	}
	if start == "" {
		start = "20200101"
	}
	if end == "" {
		end = time.Now().Format("20060102")
	}
	bars, err := s.barRepo.GetBars(code, start, end)
	if err != nil {
		return nil, err
	}
	if bars == nil {
		bars = []model.Bar{}
	}
	return bars, nil
}

// BuildSnapshots returns historical account snapshots, generating a live one
// when the database has no snapshot records.
func (s *Service) BuildSnapshots(limit int) []model.AccountSnapshot {
	tradeRepo := store.NewTradeRepo(s.db)
	snaps, err := tradeRepo.GetRecentAccountSnapshots(liveSnapshotRunID, limit)
	if err != nil {
		logger.L().Warnf("[快照查询] 失败: %v", err)
		snaps = nil
	}

	// 如果没有历史数据，用当前 portfolio 生成一个实时快照
	if len(snaps) == 0 {
		snaps = s.liveSnapshotFallback()
	}
	if snaps == nil {
		snaps = []model.AccountSnapshot{}
	}
	return snaps
}

// liveSnapshotFallback 数据库无快照记录时, 用券商实时资产生成当日快照 (仅展示用, 不落库)
func (s *Service) liveSnapshotFallback() []model.AccountSnapshot {
	date := time.Now().Format("20060102")
	// 先刷新市值
	allBars, _ := s.barRepo.GetBarsByDate(date)
	s.brk.UpdateMarketValue(barsToMap(allBars))

	asset, err := s.brk.QueryAsset()
	if err != nil || asset == nil || asset.TotalAsset <= 0 {
		return nil
	}
	initial, err := s.liveInitialCapital()
	if err != nil {
		logger.L().Warnf("[实时快照] 查询初始资金失败, 使用配置回退: %v", err)
		initial = s.cfg.Backtest.InitialCapital
	}
	snap := model.AccountSnapshot{
		TradeDate:   date,
		TotalAsset:  asset.TotalAsset,
		Cash:        asset.Cash,
		MarketValue: asset.MarketValue,
	}
	snap.FillPnL(nil, initial)
	return []model.AccountSnapshot{snap}
}
