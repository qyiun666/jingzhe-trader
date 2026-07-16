package report

// PortfolioSummary 持仓诊断汇总 (报告展示用)
type PortfolioSummary struct {
	TradeDate   string
	TotalAsset  float64
	MarketValue float64
	Cash        float64
	Concentration float64               // 集中度 (最大持仓占比)
	SectorDist  map[string]float64    // 板块分布
	HealthScore int                   // 健康度评分 0-100
	Positions   []PositionSummary
}

// PositionSummary 单只持仓展示数据
type PositionSummary struct {
	TsCode         string
	Name           string
	TotalQty       int
	CostPrice      float64
	MarketPrice    float64
	MarketValue    float64
	FloatingPnL    float64
	FloatingPnLPct float64
	WeightPct      float64
	Sector         string
	Advice         string
	RiskLevel      string
}

// RebalanceSummary 调仓计划汇总 (报告展示用)
type RebalanceSummary struct {
	TradeDate    string
	CurrentValue float64
	TargetValue  float64
	Orders       []SignalSummary
	Reason       string
}

// SignalSummary 信号展示数据
type SignalSummary struct {
	TsCode     string
	Direction  int     // 1买入 -1卖出 0持有
	TargetQty  int
	Reason     string
	Strength   float64
}

// StrategySummary 策略建议汇总 (报告展示用)
type StrategySummary struct {
	StrategyName      string
	Confidence        float64
	Reason            string
	RecommendedAction string
	RiskNote          string
}

// NewsSummary 新闻摘要
type NewsSummary struct {
	TradeDate     string
	Items         []NewsItem
	Sentiment     string
	RelatedStocks map[string][]NewsItem
}

// NewsItem 单条新闻
type NewsItem struct {
	Title     string
	Source    string
	Time      string
	Content   string
	Sentiment string
}
