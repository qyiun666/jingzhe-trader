package backtest

import "jingzhe-trader/internal/model"

// BacktestResult 回测结果
type BacktestResult struct {
	RunID          string
	StrategyName   string
	Universe       []string
	StartDate      string
	EndDate        string
	InitialCapital float64
	Metrics        Metrics
	Snapshots      []model.AccountSnapshot
	Trades         []model.Trade
}
