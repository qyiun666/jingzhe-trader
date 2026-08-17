package backtest

import "jingzhe-trader/internal/model"

// BacktestResult 回测结果
type BacktestResult struct {
	RunID              string
	StrategyName       string
	Universe           []string
	StartDate          string
	EndDate            string
	InitialCapital     float64
	Metrics            Metrics
	Snapshots          []model.AccountSnapshot
	Trades             []model.Trade
	BenchmarkName      string              // 基准代码 (未配置基准时为空)
	BenchmarkSnapshots []BenchmarkSnapshot // 基准逐日净值 (未配置基准时为空)
}

// BenchmarkSnapshot 基准逐日净值 (归一化, 起点=1)
type BenchmarkSnapshot struct {
	TradeDate string  // 交易日 YYYYMMDD
	Nav       float64 // 基准净值
}
