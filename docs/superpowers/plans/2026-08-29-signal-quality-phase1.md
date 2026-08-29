# 信号质量提升一期（资金面接入 + 辩论复盘闭环）实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 激活闲置的 moneyflow/top_list 数据喂给辩论 Agent，并建立辩论结论的事后验证闭环（复盘数据回注辩论上下文），提升买卖指令质量。

**Architecture:** 全部改动位于现有辩论管道的数据输入侧与一个新增调度任务，不触碰信号合并/风控/成交管道核心逻辑。新数据通过 `DebateContext` 可选字段流入（为空时辩论行为与现状完全一致，向后兼容）。

**Tech Stack:** Go 1.25 / sqlx / SQLite（`NewDB` 自动迁移，`:memory:` 可测试）/ zap 日志。

**参考设计:** TradingAgents v0.3 的持久化决策日志 + per-ticker 反思记忆；TradingAgents-CN 的 A 股资金面（主力资金流/龙虎榜）喂给分析师。

**明确不做（本期）:** 新闻情绪因子化（下期计划）、双模型分层、LLM 直接驱动交易。所有 LLM 产物仍走「参考输入 → 风控裁决」既有位置。

---

## 文件结构

```
internal/store/migrate.go              修改: 新增 agent_debate_review 表
internal/store/debate_review_repo.go   新建: DebateReview 模型 + 仓储
internal/store/debate_repo.go          修改: 新增 GetPendingReview
internal/store/job_repo.go             修改: 新增 JobDebateReview 常量
internal/config/config.go              修改: SchedulerConfig.DebateReviewTime + 默认值
internal/agent/types.go                修改: DebateContext 增加 3 个可选字段
internal/agent/moneyflow_view.go       新建: 资金面/龙虎榜/复盘文本格式化 helpers
internal/agent/moneyflow_view_test.go  新建: 格式化 helpers 测试
internal/agent/review.go               新建: ReviewDebates 回填逻辑 + evaluateDecision
internal/agent/review_test.go          新建: 复盘正确性判定测试
internal/agent/debate.go               修改: 构造器注入新 repo, buildContext 加载新数据
internal/agent/analysts.go             修改: TechnicalAnalyst prompt 增加资金面
internal/agent/risk_manager.go         修改: Judge prompt 注入历史复盘
internal/agent/changes_test.go         修改: 构造器调用适配（唯一测试调用方）
internal/api/handler.go                修改: 组合根注入 3 个新 repo（唯一生产调用方）
internal/scheduler/scheduler.go        修改: 注册 debate_review 每日任务
internal/scheduler/scheduler_tasks.go  修改: runDebateReview 任务实现
config/config.example.yaml             修改: 示例配置加 debate_review_time
```

**已核实的代码事实（实施者无需重查）:**
- `store.NewDB` 打开库后自动执行 `migrate`，`:memory:` 测试可用
- `BarRepo.GetBars(tsCode, start, end)` 按 `trade_date ASC` 返回
- `NewDebateOrchestrator` 调用方仅 3 处: `debate.go:31`(定义)、`handler.go:224`、`changes_test.go:20`
- `MoneyFlowRepo.GetByCode` / `TopListRepo.GetByCode` 已存在且未被任何业务消费
- `dateMinusDays(date, n)` 已存在于 `debate.go`（同包可用）
- 调度任务模式: `s.maybeRunDaily(store.JobXxx, s.cfg.Scheduler.XxxTime, now, today, s.runXxx)`（scheduler.go:131-138 交易日分支内）
- 复盘正确性判定: buy→涨为对；sell/reject→跌为对；hold 无方向**不纳入复盘**（永居 pending 无害，SQL 已过滤）

---

### Task 1: agent_debate_review 表 + DebateReviewRepo

**Files:**
- Modify: `internal/store/migrate.go`（`agent_debate` 表块之后，约 L270）
- Create: `internal/store/debate_review_repo.go`
- Test: `internal/store/debate_review_repo_test.go`

- [ ] **Step 1: 写失败测试**（`internal/store/debate_review_repo_test.go`）

```go
package store

import "testing"

func TestDebateReviewRoundtrip(t *testing.T) {
	db, err := NewDB(":memory:")
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	defer db.Close()
	repo := NewDebateReviewRepo(db)

	id, err := repo.Insert(&DebateReview{
		DebateID: 42, TradeDate: "20260820", TsCode: "600519.SH",
		Decision: "buy", Confidence: 0.7, BaseClose: 100.0,
		ReviewDate: "20260827", LastClose: 105.0, RetPct: 5.0, Correct: 1,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id=%d", id)
	}

	got, err := repo.GetRecentByCode("600519.SH", 5)
	if err != nil {
		t.Fatalf("GetRecentByCode: %v", err)
	}
	if len(got) != 1 || got[0].DebateID != 42 || got[0].RetPct != 5.0 || got[0].Correct != 1 {
		t.Fatalf("roundtrip 不匹配: %+v", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/store/ -run TestDebateReviewRoundtrip -v`
Expected: FAIL（`undefined: DebateReview`）

- [ ] **Step 3: migrate.go 加表**（在 `agent_debate` 表块、其两个索引语句之后插入）

```go
		// 辩论决策复盘 (T+5 收益回填, 反思闭环数据源; debate_id 唯一防重复回填)
		`CREATE TABLE IF NOT EXISTS agent_debate_review (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			debate_id   INTEGER NOT NULL,
			trade_date  TEXT NOT NULL,
			ts_code     TEXT NOT NULL,
			decision    TEXT NOT NULL,
			confidence  REAL NOT NULL DEFAULT 0,
			base_close  REAL NOT NULL DEFAULT 0,
			review_date TEXT NOT NULL,
			last_close  REAL NOT NULL DEFAULT 0,
			ret_pct     REAL NOT NULL DEFAULT 0,
			correct     INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_debate_review_code ON agent_debate_review(ts_code);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_debate_review_debate ON agent_debate_review(debate_id);`,
```

- [ ] **Step 4: debate_review_repo.go 实现**

```go
package store

import (
	"fmt"

	"github.com/jmoiron/sqlx"
)

// DebateReview 辩论决策复盘记录 (agent_debate 结论的事后验证)
// correct: 1=决策方向与实际涨跌一致, 0=不一致
type DebateReview struct {
	ID         int64   `json:"id" db:"id"`
	DebateID   int64   `json:"debate_id" db:"debate_id"`
	TradeDate  string  `json:"trade_date" db:"trade_date"`
	TsCode     string  `json:"ts_code" db:"ts_code"`
	Decision   string  `json:"decision" db:"decision"`
	Confidence float64 `json:"confidence" db:"confidence"`
	BaseClose  float64 `json:"base_close" db:"base_close"`
	ReviewDate string  `json:"review_date" db:"review_date"`
	LastClose  float64 `json:"last_close" db:"last_close"`
	RetPct     float64 `json:"ret_pct" db:"ret_pct"`
	Correct    int     `json:"correct" db:"correct"`
}

// DebateReviewRepo 辩论复盘仓储
type DebateReviewRepo struct {
	db *sqlx.DB
}

// NewDebateReviewRepo 构造 DebateReviewRepo
func NewDebateReviewRepo(db *sqlx.DB) *DebateReviewRepo {
	return &DebateReviewRepo{db: db}
}

// Insert 插入复盘记录 (debate_id 唯一索引兜底防重)
func (r *DebateReviewRepo) Insert(review *DebateReview) (int64, error) {
	res, err := r.db.Exec(`INSERT INTO agent_debate_review
		(debate_id, trade_date, ts_code, decision, confidence, base_close, review_date, last_close, ret_pct, correct)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		review.DebateID, review.TradeDate, review.TsCode, review.Decision, review.Confidence,
		review.BaseClose, review.ReviewDate, review.LastClose, review.RetPct, review.Correct)
	if err != nil {
		return 0, fmt.Errorf("插入辩论复盘失败: %w", err)
	}
	return res.LastInsertId()
}

// GetRecentByCode 查询指定股票最近 N 条复盘记录 (按决策日倒序, 供辩论上下文反思注入)
func (r *DebateReviewRepo) GetRecentByCode(tsCode string, limit int) ([]DebateReview, error) {
	if limit <= 0 {
		limit = 5
	}
	var reviews []DebateReview
	err := r.db.Select(&reviews,
		`SELECT * FROM agent_debate_review WHERE ts_code = ? ORDER BY trade_date DESC LIMIT ?`,
		tsCode, limit)
	if err != nil {
		return nil, fmt.Errorf("查询辩论复盘失败: %w", err)
	}
	return reviews, nil
}
```

- [ ] **Step 5: 运行测试通过**

Run: `go test ./internal/store/ -run TestDebateReviewRoundtrip -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrate.go internal/store/debate_review_repo.go internal/store/debate_review_repo_test.go
git commit -m "feat(store): 新增辩论决策复盘表与仓储"
```

---

### Task 2: GetPendingReview 查询

**Files:**
- Modify: `internal/store/debate_repo.go`（文件末尾追加）

- [ ] **Step 1: 实现**（追加到 `debate_repo.go` 末尾）

```go
// GetPendingReview 获取待复盘的辩论结论:
// 仅限有方向的决策 (buy/sell/reject, hold 无方向不可验证), 未复盘过, 决策日 <= beforeDate
func (r *DebateRepo) GetPendingReview(beforeDate string, limit int) ([]DebateResult, error) {
	if limit <= 0 {
		limit = 100
	}
	var results []DebateResult
	err := r.db.Select(&results, `SELECT d.* FROM agent_debate d
		WHERE d.trade_date <= ? AND d.decision IN ('buy','sell','reject')
		AND NOT EXISTS (SELECT 1 FROM agent_debate_review v WHERE v.debate_id = d.id)
		ORDER BY d.trade_date ASC LIMIT ?`, beforeDate, limit)
	if err != nil {
		return nil, fmt.Errorf("查询待复盘辩论失败: %w", err)
	}
	return results, nil
}
```

- [ ] **Step 2: 编译验证**

Run: `go build ./internal/store/`
Expected: 无输出（成功）

- [ ] **Step 3: Commit**

```bash
git add internal/store/debate_repo.go
git commit -m "feat(store): 新增待复盘辩论结论查询"
```

---

### Task 3: 复盘回填引擎 agent/review.go

**Files:**
- Create: `internal/agent/review.go`
- Test: `internal/agent/review_test.go`

- [ ] **Step 1: 写失败测试**（`internal/agent/review_test.go`）

```go
package agent

import "testing"

func TestEvaluateDecision(t *testing.T) {
	cases := []struct {
		decision string
		retPct   float64
		want     int
	}{
		{"buy", 1.2, 1},      // 买入后涨 → 对
		{"buy", -0.5, 0},     // 买入后跌 → 错
		{"buy", 0, 0},        // 平盘 → 错 (方向未兑现)
		{"sell", -2.0, 1},    // 卖出后跌 → 对
		{"sell", 1.0, 0},     // 卖出后涨(踏空) → 错
		{"reject", 0.1, 0},   // 否决后微涨 → 错
		{"reject", -0.1, 1},  // 否决后跌 → 对
	}
	for _, c := range cases {
		if got := evaluateDecision(c.decision, c.retPct); got != c.want {
			t.Errorf("evaluateDecision(%s, %.2f) = %d, want %d", c.decision, c.retPct, got, c.want)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/agent/ -run TestEvaluateDecision -v`
Expected: FAIL（`undefined: evaluateDecision`）

- [ ] **Step 3: 实现 review.go**

```go
package agent

import (
	"fmt"

	"jingzhe-trader/internal/store"
)

// ReviewWindowDays 辩论结论复盘窗口: 决策满 N 个自然日后回填实际收益
// (自然日窗口内若含停牌则该条留待下次, 不产生错误数据)
const ReviewWindowDays = 5

// evaluateDecision 判定决策正确性: buy→涨为对; sell/reject→跌为对
func evaluateDecision(decision string, retPct float64) int {
	switch decision {
	case "buy":
		if retPct > 0 {
			return 1
		}
	case "sell", "reject":
		if retPct < 0 {
			return 1
		}
	}
	return 0
}

// ReviewDebates 回填待复盘辩论结论的实际收益:
// base_close=决策日收盘, last_close=截至 asOf 的最新收盘, ret_pct=区间涨跌幅
// 单条数据缺失 (决策日无K线/复盘窗口内无新K线即停牌) 跳过, 留待下次复盘
// 返回本次新回填的记录
func ReviewDebates(debateRepo *store.DebateRepo, reviewRepo *store.DebateReviewRepo,
	barRepo *store.BarRepo, asOf string) ([]*store.DebateReview, error) {

	pending, err := debateRepo.GetPendingReview(dateMinusDays(asOf, ReviewWindowDays), 0)
	if err != nil {
		return nil, err
	}
	reviewed := make([]*store.DebateReview, 0, len(pending))
	for i := range pending {
		d := &pending[i]
		baseBars, err := barRepo.GetBars(d.TsCode, d.TradeDate, d.TradeDate)
		if err != nil || len(baseBars) == 0 {
			continue
		}
		history, err := barRepo.GetBars(d.TsCode, d.TradeDate, asOf)
		if err != nil || len(history) == 0 {
			continue
		}
		last := history[len(history)-1]
		if last.TradeDate == d.TradeDate {
			continue // 窗口内无新K线 (停牌/未同步), 留待下次
		}
		base := baseBars[len(baseBars)-1].Close
		retPct := 0.0
		if base > 0 {
			retPct = (last.Close - base) / base * 100
		}
		rv := &store.DebateReview{
			DebateID: d.ID, TradeDate: d.TradeDate, TsCode: d.TsCode,
			Decision: d.Decision, Confidence: d.Confidence,
			BaseClose: base, ReviewDate: asOf, LastClose: last.Close,
			RetPct: retPct, Correct: evaluateDecision(d.Decision, retPct),
		}
		if _, err := reviewRepo.Insert(rv); err != nil {
			return reviewed, fmt.Errorf("复盘落库失败 ts_code=%s debate_id=%d: %w", d.TsCode, d.ID, err)
		}
		reviewed = append(reviewed, rv)
	}
	return reviewed, nil
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/agent/ -run TestEvaluateDecision -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/review.go internal/agent/review_test.go
git commit -m "feat(agent): 辩论决策复盘回填引擎"
```

---

### Task 4: 资金面/复盘文本格式化 helpers

**Files:**
- Create: `internal/agent/moneyflow_view.go`
- Test: `internal/agent/moneyflow_view_test.go`

- [ ] **Step 1: 写失败测试**（`internal/agent/moneyflow_view_test.go`）

```go
package agent

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

func TestFormatMoneyFlows(t *testing.T) {
	if got := formatMoneyFlows(nil, 5); got != "无数据" {
		t.Fatalf("空数据应返回无数据, got %q", got)
	}
	flows := []model.MoneyFlow{
		{TradeDate: "20260825", BuyElgAmount: 900, SellElgAmount: 400, NetMFAmount: 500},
		{TradeDate: "20260826", BuyElgAmount: 800, SellElgAmount: 900, NetMFAmount: -100},
	}
	got := formatMoneyFlows(flows, 5)
	if !strings.Contains(got, "20260826") || strings.Count(got, "\n") != 2 {
		t.Fatalf("应含2条记录且最新在前: %q", got)
	}
	if strings.Contains(got, "20260825\n") {
		t.Fatalf("最新日期应在最前: %q", got)
	}
}

func TestFormatTopLists(t *testing.T) {
	if got := formatTopLists(nil, 3); got != "无" {
		t.Fatalf("空数据应返回无, got %q", got)
	}
	lists := []model.TopList{{TradeDate: "20260820", PctChange: 9.98, NetAmount: 5432}}
	got := formatTopLists(lists, 3)
	if !strings.Contains(got, "上榜") || !strings.Contains(got, "5432") {
		t.Fatalf("格式不符: %q", got)
	}
}

func TestFormatReviews(t *testing.T) {
	if got := formatReviews(nil, 5); got != "" {
		t.Fatalf("空复盘应返回空串, got %q", got)
	}
	reviews := []store.DebateReview{
		{TradeDate: "20260818", Decision: "buy", RetPct: 3.2, Correct: 1},
		{TradeDate: "20260820", Decision: "buy", RetPct: -1.5, Correct: 0},
	}
	got := formatReviews(reviews, 5)
	if !strings.Contains(got, "✓") || !strings.Contains(got, "✗") {
		t.Fatalf("应含对错标记: %q", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/agent/ -run "TestFormat" -v`
Expected: FAIL（`undefined: formatMoneyFlows`）

- [ ] **Step 3: 实现 moneyflow_view.go**

```go
package agent

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// formatMoneyFlows 近 n 个交易日资金流文本 (最新在前); 空数据返回占位文案
func formatMoneyFlows(flows []model.MoneyFlow, n int) string {
	if len(flows) == 0 {
		return "无数据"
	}
	var sb strings.Builder
	start := 0
	if len(flows) > n {
		start = len(flows) - n
	}
	for i := len(flows) - 1; i >= start; i-- {
		f := flows[i]
		sb.WriteString(fmt.Sprintf("  %s 净流入%.0f万 (超大单买%.0f万/卖%.0f万)\n",
			f.TradeDate, f.NetMFAmount, f.BuyElgAmount, f.SellElgAmount))
	}
	return sb.String()
}

// formatTopLists 龙虎榜文本 (最新在前); 空数据返回占位文案
func formatTopLists(lists []model.TopList, n int) string {
	if len(lists) == 0 {
		return "无"
	}
	var sb strings.Builder
	start := 0
	if len(lists) > n {
		start = len(lists) - n
	}
	for i := len(lists) - 1; i >= start; i-- {
		t := lists[i]
		sb.WriteString(fmt.Sprintf("  %s 上榜 涨跌%.2f%% 净买入%.0f万\n",
			t.TradeDate, t.PctChange, t.NetAmount))
	}
	return sb.String()
}

// formatReviews 历史辩论复盘文本 (反思注入); 空数据返回空串 (调用方据此省略整个小节)
func formatReviews(reviews []store.DebateReview, n int) string {
	if len(reviews) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range reviews {
		if i >= n {
			break
		}
		verdict := "✗"
		if r.Correct == 1 {
			verdict = "✓"
		}
		sb.WriteString(fmt.Sprintf("  %s 决策=%s → 后续%.2f%% %s\n",
			r.TradeDate, r.Decision, r.RetPct, verdict))
	}
	return sb.String()
}
```

- [ ] **Step 4: 运行测试通过**

Run: `go test ./internal/agent/ -run "TestFormat" -v`
Expected: PASS（3 个测试全绿）

- [ ] **Step 5: Commit**

```bash
git add internal/agent/moneyflow_view.go internal/agent/moneyflow_view_test.go
git commit -m "feat(agent): 资金面/龙虎榜/复盘文本格式化"
```

---

### Task 5: DebateContext 扩展 + Orchestrator 注入 + buildContext 加载

**Files:**
- Modify: `internal/agent/types.go`（DebateContext）
- Modify: `internal/agent/debate.go`（结构体字段、构造器、buildContext）
- Modify: `internal/agent/changes_test.go:20`（构造器调用适配）
- Modify: `internal/api/handler.go:224`（组合根注入）——本任务先改 agent 包并编译，handler 适配放在 Task 5 一并完成（否则编译不过）

- [ ] **Step 1: types.go 扩展 DebateContext**

```go
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
```

- [ ] **Step 2: debate.go 结构体与构造器**

```go
type DebateOrchestrator struct {
	llm           *llm.Client
	barRepo       *store.BarRepo
	basicRepo     *store.BasicRepo
	finaRepo      *store.FinaRepo
	newsRepo      *store.NewsRepo
	debateRepo    *store.DebateRepo
	reviewRepo    *store.DebateReviewRepo
	moneyflowRepo *store.MoneyFlowRepo
	toplistRepo   *store.TopListRepo
	tech          *TechnicalAnalyst
	fund          *FundamentalAnalyst
	news          *NewsAnalyst
	market        *MarketAnalyst
	bull          *researcher
	bear          *researcher
	riskMgr       *RiskManagerAgent
}

func NewDebateOrchestrator(llmClient *llm.Client, barRepo *store.BarRepo, basicRepo *store.BasicRepo,
	finaRepo *store.FinaRepo, newsRepo *store.NewsRepo, debateRepo *store.DebateRepo,
	reviewRepo *store.DebateReviewRepo, moneyflowRepo *store.MoneyFlowRepo, toplistRepo *store.TopListRepo) *DebateOrchestrator {
	o := &DebateOrchestrator{llm: llmClient, barRepo: barRepo, basicRepo: basicRepo, finaRepo: finaRepo,
		newsRepo: newsRepo, debateRepo: debateRepo, reviewRepo: reviewRepo,
		moneyflowRepo: moneyflowRepo, toplistRepo: toplistRepo}
	// ... (后续 analyst/researcher 初始化保持不变)
```

- [ ] **Step 3: buildContext 加载新数据**（`return &DebateContext{...}` 前插入，并替换 return）

```go
	// 资金面数据 (近2周): 数据缺失不影响辩论, 仅上下文变薄
	var flows []model.MoneyFlow
	if o.moneyflowRepo != nil {
		if f, err := o.moneyflowRepo.GetByCode(tsCode, dateMinusDays(date, 14), date); err == nil {
			flows = f
		}
	}
	var tops []model.TopList
	if o.toplistRepo != nil {
		if t, err := o.toplistRepo.GetByCode(tsCode, dateMinusDays(date, 14), date); err == nil {
			tops = t
		}
	}
	// 历史辩论复盘 (反思闭环: 最近5次有方向决策的实际结果)
	var reviewSummary string
	if o.reviewRepo != nil {
		if reviews, err := o.reviewRepo.GetRecentByCode(tsCode, 5); err == nil && len(reviews) > 0 {
			reviewSummary = formatReviews(reviews, 5)
		}
	}
	return &DebateContext{TradeDate: date, TsCode: tsCode, Name: name, Bars: history,
		Position: pos, TotalAsset: totalAsset, MarketBars: marketBars,
		MoneyFlows: flows, TopLists: tops, ReviewSummary: reviewSummary}
```

- [ ] **Step 4: 适配两处调用方**

`internal/agent/changes_test.go:20`:
```go
	return NewDebateOrchestrator(nil, nil, nil, nil, nil, repo, nil, nil, nil), repo
```

`internal/api/handler.go`（原 `agent.NewDebateOrchestrator(...)` 调用，追加 3 个新 repo）:
```go
	// 初始化智能体辩论编排器 (复用 Service 的共享 Repo, 避免重复实例化)
	svc.debateOrchestrator = agent.NewDebateOrchestrator(
		svc.llmClient,
		svc.barRepo,
		svc.basicRepo,
		svc.finaRepo,
		svc.newsRepo,
		svc.debateRepo,
		store.NewDebateReviewRepo(db),
		store.NewMoneyFlowRepo(db),
		store.NewTopListRepo(db),
	)
```

- [ ] **Step 5: 编译 + 全量 agent/api 测试**

Run: `go build ./... && go test ./internal/agent/ ./internal/api/ 2>&1 | tail -20`
Expected: 编译成功，测试 PASS

- [ ] **Step 6: Commit**

```bash
git add internal/agent/types.go internal/agent/debate.go internal/agent/changes_test.go internal/api/handler.go
git commit -m "feat(agent): 辩论上下文接入资金面与复盘数据"
```

---

### Task 6: TechnicalAnalyst prompt 接入资金面

**Files:**
- Modify: `internal/agent/analysts.go`（TechnicalAnalyst.Analyze 的 userPrompt 与 technicalSysPrompt）

- [ ] **Step 1: userPrompt 增加资金面小节**

```go
	flowText := formatMoneyFlows(ctx.MoneyFlows, 5)
	topText := formatTopLists(ctx.TopLists, 3)
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
近5日K线:
%s
最新收盘: %.2f  前收: %.2f  涨跌幅: %.2f%%
MA5: %s  MA20: %s  (MA5%sMA20)
RSI(14): %s  (RSI>70超买, <30超卖)
成交量比(5日均量): %.2f
资金面(近5个交易日):
%s
龙虎榜(近2周):
%s
总资产: %.0f  持仓: %s

请从技术面分析该股票，输出JSON:
{"sentiment": -1到1, "key_points": ["要点1","要点2"], "risks": ["风险1"], "confidence": 0到1}`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		formatRecentBars(bars, 5),
		last.Close, last.PreClose, last.PctChg,
		ma5Str, ma20Str, crossIndicator,
		rsiStr,
		volRatio,
		flowText, topText,
		ctx.TotalAsset, posStr(ctx.Position))
```

- [ ] **Step 2: technicalSysPrompt 分析框架追加第5条**

```go
4. 支撑阻力：近期高低点形成的关键位
5. 资金面：主力净流入连续为正=资金入场，连续净流出=主力撤退；上龙虎榜的股票短期波动放大，需警惕游资一日游
```

- [ ] **Step 3: 测试 + 编译**

Run: `go test ./internal/agent/ 2>&1 | tail -5 && go build ./...`
Expected: PASS，编译成功

- [ ] **Step 4: Commit**

```bash
git add internal/agent/analysts.go
git commit -m "feat(agent): 技术分析师prompt接入资金面数据"
```

---

### Task 7: 风险经理裁决注入历史复盘

**Files:**
- Modify: `internal/agent/risk_manager.go`（Judge 的 userPrompt）

- [ ] **Step 1: prompt 注入复盘小节**（`reportsText` 拼装前插入，并修改 userPrompt 模板）

```go
	reviewSection := ""
	if ctx.ReviewSummary != "" {
		reviewSection = fmt.Sprintf("\n该股历史辩论复盘 (近期有方向决策的实际结果):\n%s请特别参考: 若此前 buy 决策多次亏损, 本次应更保守; 若 sell/reject 后多次踏空上涨, 对看空论点要求更严格证据。\n", ctx.ReviewSummary)
	}
	userPrompt := fmt.Sprintf(`股票: %s (%s)  日期: %s
当前持仓: %s  总资产: %.0f
%s
分析师报告:
%s
...（模板其余部分不变）`,
		ctx.TsCode, ctx.Name, ctx.TradeDate,
		posStr(ctx.Position), ctx.TotalAsset,
		reviewSection,
		reportsText, bullText, bearText)
```

- [ ] **Step 2: 测试 + 编译**

Run: `go test ./internal/agent/ 2>&1 | tail -5 && go build ./...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add internal/agent/risk_manager.go
git commit -m "feat(agent): 风控裁决注入历史辩论复盘(反思闭环)"
```

---

### Task 8: 调度任务 debate_review + 配置

**Files:**
- Modify: `internal/store/job_repo.go:18-28`（常量块）
- Modify: `internal/config/config.go`（SchedulerConfig + Load 默认值）
- Modify: `internal/scheduler/scheduler.go:135`（注册）
- Modify: `internal/scheduler/scheduler_tasks.go`（任务实现）
- Modify: `config/config.example.yaml`

- [ ] **Step 1: job 常量**

```go
	JobSettleT1      = "settle_t1"
	JobPremarket     = "premarket"
	JobDebateReview  = "debate_review"
```

- [ ] **Step 2: config.go SchedulerConfig 增字段**（ReportTime 之后）

```go
	ReportTime       string         `mapstructure:"report_time"`       // 日报生成时间 HH:MM
	DebateReviewTime string         `mapstructure:"debate_review_time"` // 辩论复盘回填时间 HH:MM
```

`Load` 中 scheduler 默认值处（与其他 scheduler.SetDefault 相邻）追加:
```go
	v.SetDefault("scheduler.debate_review_time", "15:20")
```
（若 config.go 中无 scheduler 前缀的 SetDefault，则新增一行即可，viper 未配置字段回退默认值）

- [ ] **Step 3: scheduler.go 注册**（`s.maybeRunDaily(store.JobScreener, ...)` 之后一行）

```go
		s.maybeRunDaily(store.JobDebateReview, s.cfg.Scheduler.DebateReviewTime, now, today, s.runDebateReview)
```

- [ ] **Step 4: scheduler_tasks.go 实现**（`runScreener` 之后追加；import 块增加 `"jingzhe-trader/internal/agent"`）

```go
// runDebateReview 辩论决策复盘: 回填满窗口期(5自然日)辩论结论的实际收益
// 反思闭环数据源: 复盘结果通过 DebateContext.ReviewSummary 注入后续辩论
func (s *Scheduler) runDebateReview(date string) error {
	reviewed, err := agent.ReviewDebates(
		store.NewDebateRepo(s.db),
		store.NewDebateReviewRepo(s.db),
		store.NewBarRepo(s.db),
		date,
	)
	if err != nil {
		return fmt.Errorf("辩论复盘失败: %w", err)
	}
	if len(reviewed) == 0 {
		return nil
	}
	correct := 0
	for _, r := range reviewed {
		if r.Correct == 1 {
			correct++
		}
	}
	logger.L().Infow("辩论复盘回填完成", "date", date, "reviewed", len(reviewed), "correct", correct)
	s.alert("🧠 惊蛰辩论复盘", fmt.Sprintf(
		"%s 回填 %d 条辩论结论验证: 命中 %d/%d (%.0f%%)\n复盘数据将注入后续辩论上下文 (反思闭环)",
		date, len(reviewed), correct, len(reviewed), float64(correct)/float64(len(reviewed))*100))
	return nil
}
```

- [ ] **Step 5: config.example.yaml scheduler 段追加**

```yaml
  # 辩论复盘回填时间 (辩论结论满5个自然日后回填实际收益, 反思闭环)
  debate_review_time: "15:20"
```

- [ ] **Step 6: 编译 + 全量测试**

Run: `go build ./... && go test ./... 2>&1 | tail -30`
Expected: 全部 PASS

- [ ] **Step 7: Commit**

```bash
git add internal/store/job_repo.go internal/config/config.go internal/scheduler/scheduler.go internal/scheduler/scheduler_tasks.go config/config.example.yaml
git commit -m "feat(scheduler): 新增辩论复盘每日回填任务"
```

---

### Task 9: 端到端验证

- [ ] **Step 1: 全量构建与静态检查**

Run: `make build && make vet && go test ./... 2>&1 | tail -30`
Expected: 无 FAIL

- [ ] **Step 2: 冒烟验证复盘 SQL（对现有库只读检查）**

Run: `sqlite3 data/jingzhe.db "SELECT COUNT(*) FROM agent_debate WHERE decision IN ('buy','sell','reject');"`
Expected: 返回数字（历史待复盘存量，首次运行 15:20 任务时回填）

- [ ] **Step 3: 数据库迁移冒烟（不启动完整服务）**

Run: `go run ./cmd/server -config config/config.yaml &  sleep 3 && sqlite3 data/jingzhe.db ".schema agent_debate_review" && kill %1`
Expected: schema 输出包含 `idx_debate_review_debate`；旧数据无破坏

- [ ] **Step 4: 最终 Commit（如有遗留）+ 汇报**

```bash
git status --short   # 确认无遗漏文件
```

---

## 风险与回滚

- **向后兼容**: 所有新数据为可选字段，DB 无数据时辩论 prompt 与现状一致；`agent_debate_review` 为纯新增表，不影响现有表
- **幂等**: `debate_id` 唯一索引 + `NOT EXISTS` 双重防重；任务失败次日自动重试（pending 仍在）
- **回滚**: 每个任务独立 commit，`git revert` 单个提交即可回滚对应功能；新表可保留不删
- **成本**: 每次辩论 prompt 略增（约 +200 token 资金面/复盘文本），LLM 调用次数不变
