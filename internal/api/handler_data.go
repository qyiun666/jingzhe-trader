package api

import (
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
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
	var snaps []model.AccountSnapshot
	query := `SELECT trade_date, total_asset, cash, market_value, pnl, pnl_pct, total_pnl, total_pnl_pct
	          FROM account_snapshot ORDER BY trade_date DESC LIMIT ?`
	if err := s.db.Select(&snaps, query, limit); err != nil {
		snaps = []model.AccountSnapshot{}
	}
	if snaps == nil {
		snaps = []model.AccountSnapshot{}
	}

	// 如果没有历史数据，用当前 portfolio 生成一个实时快照
	if len(snaps) == 0 {
		// 先刷新市值
		date := time.Now().Format("20060102")
		allBars, _ := s.barRepo.GetBarsByDate(date)
		todayBars := barsToMap(allBars)
		s.brk.UpdateMarketValue(todayBars)

		asset, _ := s.brk.QueryAsset()
		if asset != nil && asset.TotalAsset > 0 {
			var totalPnL, totalPnLPct float64
			portfolioRepo := store.NewPortfolioRepo(s.db)
			initialStr, _ := portfolioRepo.GetMeta("initial_capital")
			if initialStr != "" {
				var ic float64
				fmt.Sscanf(initialStr, "%f", &ic)
				if ic > 0 {
					totalPnL = asset.TotalAsset - ic
					totalPnLPct = totalPnL / ic
				}
			}
			snaps = append(snaps, model.AccountSnapshot{
				TradeDate:   date,
				TotalAsset:  asset.TotalAsset,
				Cash:        asset.Cash,
				MarketValue: asset.MarketValue,
				TotalPnL:    totalPnL,
				TotalPnLPct: totalPnLPct,
			})
		}
	}

	// 反转为升序
	for i, j := 0, len(snaps)-1; i < j; i, j = i+1, j-1 {
		snaps[i], snaps[j] = snaps[j], snaps[i]
	}
	return snaps
}
