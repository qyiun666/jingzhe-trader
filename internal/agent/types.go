package agent

import (
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// DebateResult 辩论结果 (定义在 store 包, 此处别名, 避免循环依赖)
type DebateResult = store.DebateResult

// AnalysisReport 分析师报告
type AnalysisReport struct {
	Agent      string   `json:"agent"`
	TsCode     string   `json:"ts_code"`
	Sentiment  float64  `json:"sentiment"`
	KeyPoints  []string `json:"key_points"`
	Risks      []string `json:"risks"`
	Confidence float64  `json:"confidence"`
}

// DebateContext 辩论上下文
type DebateContext struct {
	TradeDate     string
	TsCode        string
	Name          string
	Bars          []model.Bar
	Position      *model.Position
	TotalAsset    float64
	MarketBars    map[string]*model.Bar
	MoneyFlows    []model.MoneyFlow // 近期资金流向 (nil=无数据, 辩论照常进行)
	TopLists      []model.TopList   // 近期龙虎榜 (nil=无数据)
	ReviewSummary string            // 历史辩论复盘文本 (空=无历史)
}

// Analyst 分析师接口
type Analyst interface {
	Name() string
	Analyze(ctx *DebateContext) (*AnalysisReport, error)
}

// Researcher 研究员接口
type Researcher interface {
	Name() string
	Research(ctx *DebateContext, reports []*AnalysisReport) (*ResearchArgument, error)
}

// ResearchArgument 研究员论点
type ResearchArgument struct {
	Side       string   `json:"side"`
	Sentiment  float64  `json:"sentiment"`
	Arguments  []string `json:"arguments"`
	Confidence float64  `json:"confidence"`
}

// 决策变更类型
const (
	ChangeTypeDecision  = "decision"   // 真正的决策/置信度/风险等级变更
	ChangeTypeNewSymbol = "new_symbol" // 新增标的(此前无辩论记录)
)

// DecisionChange 决策变更记录
type DecisionChange struct {
	Type           string  `json:"type"` // 变更类型: decision / new_symbol
	TsCode         string  `json:"ts_code"`
	Name           string  `json:"name"`
	PrevDecision   string  `json:"prev_decision"`
	CurrDecision   string  `json:"curr_decision"`
	PrevConfidence float64 `json:"prev_confidence"`
	CurrConfidence float64 `json:"curr_confidence"`
	Changed        bool    `json:"changed"`
	Detail         string  `json:"detail"`
}
