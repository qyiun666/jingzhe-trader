package model

// ===================== 运维域模型 =====================

// JobRun 任务执行记录：四态（running/success/degraded/failed）+ 产出物契约。
type JobRun struct {
	ID          int64    `db:"id"`
	JobName     string   `db:"job_name"`
	TradeDate   string   `db:"trade_date"`
	Attempt     int      `db:"attempt"`
	Status      string   `db:"status"`
	DurationMs  int64    `db:"duration_ms"`
	Error       string   `db:"error"`
	Artifacts   string   `db:"artifacts"`   // JSON：产出物声明与实测值
	Degradations string  `db:"degradations"` // JSON：降级/跳过记录
	StartedAt   string   `db:"started_at"`
	FinishedAt  string   `db:"finished_at"`
}

// AgentAlert 告警：四级 + 已读标记 + 告警码（聚合去重与单测断言用）。
type AgentAlert struct {
	ID        int64      `db:"id"`
	TradeDate string     `db:"trade_date"`
	Source    string     `db:"source"`
	Level     AlertLevel `db:"level"`
	Code      string     `db:"code"`
	Title     string     `db:"title"`
	Content   string     `db:"content"`
	Status    string     `db:"status"`
	CreatedAt string     `db:"created_at"`
	ReadAt    string     `db:"read_at"`
}

// ActionLog 审计日志：谁/何时/改了什么/前后值。
type ActionLog struct {
	ID         int64  `db:"id"`
	TradeDate  string `db:"trade_date"`
	Actor      string `db:"actor"`
	ObjectType string `db:"object_type"`
	ObjectID   string `db:"object_id"`
	Action     string `db:"action"`
	BeforeValue string `db:"before_value"`
	AfterValue  string `db:"after_value"`
	Reason     string `db:"reason"`
	CreatedAt  string `db:"created_at"`
}

// MailOutbox 邮件发件箱：破解"任务全绿但零邮件"的关键表（D1）。
type MailOutbox struct {
	ID          int64    `db:"id"`
	TradeDate   string   `db:"trade_date"`
	MailType    MailType `db:"mail_type"`
	Subject     string   `db:"subject"`
	Body        string   `db:"body"`
	Status      string   `db:"status"`
	Attempts    int      `db:"attempts"`
	LastError   string   `db:"last_error"`
	NextRetryAt string   `db:"next_retry_at"`
	CreatedAt   string   `db:"created_at"`
	SentAt      string   `db:"sent_at"`
}

// LLMCall LLM 终审调用记录（P1，失败显式落库，绝不把失败标成"已分析"）。
type LLMCall struct {
	ID           int64   `db:"id"`
	TradeDate    string  `db:"trade_date"`
	SignalID     int64   `db:"signal_id"`
	TsCode       string  `db:"ts_code"`
	Verdict      string  `db:"verdict"`
	Confidence   float64 `db:"confidence"`
	Rationale    string  `db:"rationale"`
	Status       string  `db:"status"`
	Error        string  `db:"error"`
	ReviewDate   string  `db:"review_date"`
	ReviewRetPct float64 `db:"review_ret_pct"`
	ReviewCorrect int    `db:"review_correct"`
	CreatedAt    string  `db:"created_at"`
}

// ===================== 目标域模型 =====================

// GoalState 档位状态机持久化（单行 id=1）。
type GoalState struct {
	Quarter          string `db:"quarter"`
	QuarterStart     string `db:"quarter_start"`
	QuarterEnd       string `db:"quarter_end"`
	BaselineAsset    Fen    `db:"baseline_asset"`
	PeakAsset        Fen    `db:"peak_asset"`
	CurrentGear      Gear   `db:"current_gear"`
	ProfitLock       bool   `db:"profit_lock"`
	UpgradeStreak    int    `db:"upgrade_streak"`
	LastEvalDate     string `db:"last_eval_date"`
	OverrideGear     string `db:"override_gear"`
	OverrideReason   string `db:"override_reason"`
	OverrideUntil    string `db:"override_until"`
	PacePolicy       string `db:"pace_policy"`
	PaceConfirmDate  string `db:"pace_confirm_date"`
	UpdatedAt        string `db:"updated_at"`
}

// GoalGearLog 档位变更日志（可回放）。
type GoalGearLog struct {
	ID             int64   `db:"id"`
	TradeDate      string  `db:"trade_date"`
	Quarter        string  `db:"quarter"`
	FromGear       Gear    `db:"from_gear"`
	ToGear         Gear    `db:"to_gear"`
	FromLock       bool    `db:"from_lock"`
	ToLock         bool    `db:"to_lock"`
	TriggerRule    string  `db:"trigger_rule"`
	Progress       float64 `db:"progress"`
	BudgetConsumed float64 `db:"budget_consumed"`
	PaceGap        float64 `db:"pace_gap"`
	IsManual       bool    `db:"is_manual"`
	Reason         string  `db:"reason"`
	ParamsSnapshot string  `db:"params_snapshot"`
	CreatedAt      string  `db:"created_at"`
}
