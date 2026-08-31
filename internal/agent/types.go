package agent

import (
	"strings"

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

// missingDataPrefix 标记该分析师报告无有效依据, 不计入均值
// 用它而不是 Confidence=0.1: 后者与模型真实给出的 0.1 无法区分, 会把"没有意见"
// 当成一个偏空的意见计入风控的平均 sentiment
const missingDataPrefix = "数据缺失:"

// reportMissingData LLM 调用失败时的占位要点
const reportMissingData = missingDataPrefix + " 本报告无有效依据, 不计入均值"

// noDataReport 该分析维度无可用输入 (无新闻/无基本面/无指数行情) 时的占位报告
// 与"分析失败"同样不参与均值: 没有消息面 ≠ 消息面偏空
func noDataReport(agent, tsCode, reason string) *AnalysisReport {
	return &AnalysisReport{
		Agent:     agent,
		TsCode:    tsCode,
		KeyPoints: []string{missingDataPrefix + " " + reason},
	}
}

// IsMissingData 判断报告是否为无有效依据的占位报告
func (r *AnalysisReport) IsMissingData() bool {
	if r == nil {
		return true
	}
	for _, p := range r.KeyPoints {
		if strings.HasPrefix(p, missingDataPrefix) {
			return true
		}
	}
	return false
}

// DebateContext 辩论上下文
type DebateContext struct {
	TradeDate     string
	TsCode        string
	Name          string
	Industry      string // 所属行业; 个股级新闻在本账号数据档位下取不到, 行业消息是唯一可用输入
	Bars          []model.Bar
	Position      *model.Position
	TotalAsset    float64
	MoneyFlows    []model.MoneyFlow // 近期资金流向 (nil=无数据, 辩论照常进行)
	TopLists      []model.TopList   // 近期龙虎榜 (nil=无数据)
	ReviewSummary string            // 历史辩论复盘文本 (空=无历史)
}

// Analyst 分析师接口
type Analyst interface {
	Name() string
	Analyze(ctx *DebateContext) (*AnalysisReport, error)
}

// clamp 把 LLM 返回的数值压回合法区间
// 下游 (风控均值/仓位换算/置信度排序) 都按取值范围解释这些数字, 越界只会静默放大结果
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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
