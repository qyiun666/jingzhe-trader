package analysis

// NewsSummary 新闻摘要
type NewsSummary struct {
	TradeDate     string
	Items         []NewsItem
	Sentiment     string // positive/negative/neutral
	RelatedStocks map[string][]NewsItem
}

// NewsItem 单条新闻
type NewsItem struct {
	Title     string
	Source    string
	Time      string
	Content   string
	Sentiment string // positive/negative/neutral
}
