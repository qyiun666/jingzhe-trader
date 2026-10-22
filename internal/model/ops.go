package model

// ===================== 轨迹模型 =====================

// 轨迹结果三态。partial = 做成了但有已知缺失（原 degraded 语义）。
const (
	TraceOK      = "ok"
	TracePartial = "partial"
	TraceFail    = "fail"
)

// RunTrace 轨迹：一件事在一个交易日只留一行最终结果。
//
// Subject 是"这件事"的稳定标识，前缀区分来源，同一 subject 当日重复写入即覆盖：
//
//	job:<任务名> / mail:<邮件类型> / alert:<告警码> / llm:<标的>:<prompt_key> / screen:topn / portfolio:snapshot
//
// 每个字段的读者：TradeDate=当日轨迹查询与保留窗口；Subject=完成判定与去重判定；
// Outcome=调度器 shouldSkip 与 MCP/selfcheck 的唯一判据；Detail=失败时给人看的那一句；
// At=排序与冷却判定。不存尝试次数、耗时、产出物明细 —— 那些是过程，不是结果。
// 唯一的产出物明细是 screen:topn 的候选快照，见 TraceScreenTopN。
type RunTrace struct {
	ID        int64  `db:"id"`
	TradeDate string `db:"trade_date"`
	Subject   string `db:"subject"`
	Outcome   string `db:"outcome"`
	Detail    string `db:"detail"`
	At        string `db:"at"`
}

// Subject 构造函数：subject 是 run_trace 的查找键（完成判定、冷却、去重、回答缓存都按它查），
// 集中在这里拼，避免各调用方手拼前缀拼错后静默写出查不到的行。
func TraceJob(name string) string              { return "job:" + name }
func TraceMail(typ MailType) string            { return "mail:" + string(typ) }
func TraceAlert(code string) string            { return "alert:" + code }
func TraceLLM(tsCode, promptKey string) string { return "llm:" + tsCode + ":" + promptKey }

// TraceScreenTopN 每日候选快照的 subject（一天一行，重跑覆盖）。
// 这是"选股过程不落库"的唯一例外：归因需要当时 TopN 的 score/factors 才能
// 回答"高分候选后来是否真的更好"，而决策留痕只有裁决、没有候选截面。
func TraceScreenTopN() string { return "screen:topn" }

// TracePortfolioSnapshot 每日组合快照的 subject（一天一行，重跑覆盖）。
// M5 的组合 5/10/20 日涨跌只从这些快照现算；不建资产曲线表、不加邮件通道。
func TracePortfolioSnapshot() string { return "portfolio:snapshot" }

// PortfolioSnapshot 每日组合快照（run_trace.detail 的 JSON 载荷）。
// 资产为当日收盘口径；指数收盘用于计算相对 000300 的超额。
type PortfolioSnapshot struct {
	TradeDate      string `json:"trade_date"`
	TotalAssetFen  int64  `json:"total_asset_fen"`
	CashFen        int64  `json:"cash_fen"`
	MarketValueFen int64  `json:"market_value_fen"`
	PositionCount  int    `json:"position_count"`
	IndexCloseFen  int64  `json:"index_close_fen"` // 000300.SH 当日收盘（分）；缺失为 0
}

// ScreenSnapshot 每日候选快照（run_trace.detail 的 JSON 载荷）。
// 只存归因用得到的字段：代码、评分、因子、行业与当日资金口径；
// 不存理由/估值明细——那些在日志与 LLM 留痕里，重复存会撑大轨迹表。
type ScreenSnapshot struct {
	TradeDate     string              `json:"trade_date"`
	TotalAssetFen int64               `json:"total_asset_fen"`
	SlotFen       int64               `json:"slot_fen"` // 当晚生效的单票上限（弱势档为收缩后的值）
	Candidates    []CandidateSnapshot `json:"candidates"`
}

// CandidateSnapshot 单只候选的快照。
type CandidateSnapshot struct {
	TsCode    string  `json:"ts_code"`
	Name      string  `json:"name"`
	Industry  string  `json:"industry"`
	Kind      string  `json:"kind"` // 空=个股，etf=场内 ETF 补口
	Rank      int     `json:"rank"`
	PoolSize  int     `json:"pool_size"`
	Score     float64 `json:"score"`
	Momentum  float64 `json:"momentum"`
	Value     float64 `json:"value"`
	LowVol    float64 `json:"low_vol"`
	Liquidity float64 `json:"liquidity"`
	CloseFen  int64   `json:"close_fen"`
	Mom       float64 `json:"mom"`
	SectorMom float64 `json:"sector_mom"`
}

// LLMCall 一条 prompt 对一只票的回答，存成一整行 run_trace（见 store.LLMRepo）：
// Subject=TraceLLM(标的,prompt_key)、Outcome=Status、Detail=其余字段序列化。
//
// 读者是决策链自己 —— 重跑当日先查这一行，ok 就直接复用，不再花第二次钱；
// Status 只有 TraceOK / TraceFail：失败行不算缓存命中，当天重跑会重试它，
// 否则一次网络抖动就把这只票当天锁死。
// Verdict 按 prompt 取值：证据行 positive / neutral / negative / unknown，
// 决策行（prompt_key='decision'）buy / skip。
type LLMCall struct {
	TradeDate  string
	TsCode     string
	PromptKey  string
	Verdict    string
	Confidence float64
	WeightPct  float64 // 仅决策行：模型批的仓位比例（占总资产）
	Rationale  string
	Status     string // = run_trace.outcome
	Error      string
	CreatedAt  string // = run_trace.at
}
