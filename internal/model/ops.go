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
//	job:<任务名> / mail:<邮件类型> / alert:<告警码>
//
// 每个字段的读者：TradeDate=当日轨迹查询与保留窗口；Subject=完成判定与去重判定；
// Outcome=调度器 shouldSkip 与 MCP/selfcheck 的唯一判据；Detail=失败时给人看的那一句；
// At=排序与冷却判定。不存尝试次数、耗时、产出物明细 —— 那些是过程，不是结果。
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

// TraceCalib 决策归因行的 subject（trade_date = 决策日）。一天一只票一行，
// 后续收益到期后原地覆盖。它与 llm:<标的>:decision 行分开命名空间：那一行是"模型说了什么"，
// 会被当日重跑重写；这一行是"事后证明对不对"，只由归因任务写，两者互不覆盖。
func TraceCalib(tsCode string) string { return "cal:" + tsCode }

// Calibration 一条买入决策的事后归因（置信度校准的唯一数据源）。
//
// 存在理由：LLM 自报的 Confidence 是决策链唯一的软门槛依据（G1 档 0.55），
// 但没有任何机制证明过它与真实收益相关。这一结构记录"当时说了什么 + 事后涨跌"，
// 让"高置信度是否真有更高胜率"变成一个能用数据回答的问题。
//
// 三个观察期都是**交易日**偏移（5/10/20），收益为前复权口径小数（0.03 = +3%）。
// 未到期时 filled=false，收益值为 0 且不参与统计。
type Calibration struct {
	TradeDate  string  `db:"trade_date"` // 决策日
	TsCode     string  `db:"ts_code"`
	Verdict    string  `db:"verdict"` // buy / skip（模型当时的态度）
	Confidence float64 `db:"confidence"`
	WeightPct  float64 `db:"weight_pct"`
	Ret5       float64 `db:"ret5"`
	Ret10      float64 `db:"ret10"`
	Ret20      float64 `db:"ret20"`
	Has5       bool    `db:"has5"`
	Has10      bool    `db:"has10"`
	Has20      bool    `db:"has20"`
	At         string  `db:"at"`
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

// ===================== 目标域模型 =====================

// GoalState 档位状态机的当前状态。
//
// 存法：整个结构序列化成 JSON，落在 config_kv 的 goal.state 一个键上（见 store.GoalRepo）。
// 它不配独立一张表的理由是"只有一行、且每次写都整行覆盖"——没有任何调用方按列更新它，
// 字段只是 Go 结构体的成员名，从来不是查询条件。
//
// json tag 沿用原列名：改字段名会让已落库的 JSON 键一起漂，所以只加 tag 不改名。
type GoalState struct {
	Quarter         string `json:"quarter"`
	QuarterStart    string `json:"quarter_start"`
	QuarterEnd      string `json:"quarter_end"`
	BaselineAsset   Fen    `json:"baseline_asset"`
	PeakAsset       Fen    `json:"peak_asset"`
	CurrentGear     Gear   `json:"current_gear"`
	ProfitLock      bool   `json:"profit_lock"`
	UpgradeStreak   int    `json:"upgrade_streak"`
	LastEvalDate    string `json:"last_eval_date"`
	OverrideGear    string `json:"override_gear"`
	OverrideReason  string `json:"override_reason"`
	OverrideUntil   string `json:"override_until"`
	PacePolicy      string `json:"pace_policy"`
	PaceConfirmDate string `json:"pace_confirm_date"`
	UpdatedAt       string `json:"updated_at"`
}
