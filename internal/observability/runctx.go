package observability

import (
	"context"
	"fmt"
	"sort"
)

// ArtifactCode 产出物缺失告警码（D1）。
const ArtifactCode = "ARTIFACT_MISSING"

// Artifact 产出物声明：本任务应当产出什么（kind 分类，name 具体名，expect 期望数量）。
type Artifact struct {
	Kind   string
	Name   string
	Expect int // 期望数量；-1 表示"至少 1 个"
	Actual int
}

// Degradation 降级/跳过记录（任何因数据/配置/依赖缺失而跳过路径的唯一出口）。
type Degradation struct {
	Code   string
	Reason string
}

// ArtifactMiss 断言失败时的一条产出物缺失记录。
type ArtifactMiss struct {
	Kind   string
	Name   string
	Expect int
	Actual int
	Code   string
}

// RunCtx 产出物契约（D1 核心机制）。每个任务一个，用于杜绝"任务全绿但什么都没发生"。
//
//	Declare → Actual → （必要时 Degrade）→ Assert
//
// 唯一跳过出口是 Degrade()；禁止在任务函数里直接 return nil 静默跳过。
type RunCtx struct {
	ctx        context.Context
	jobName    string
	tradeDate  string
	artifacts  map[string]*Artifact
	order      []string
	degrades   []Degradation
	degraded   bool
}

// NewRunCtx 创建任务产出物上下文。
func NewRunCtx(ctx context.Context, jobName, tradeDate string) *RunCtx {
	if ctx == nil {
		ctx = context.Background()
	}
	return &RunCtx{
		ctx:       ctx,
		jobName:   jobName,
		tradeDate: tradeDate,
		artifacts: make(map[string]*Artifact),
	}
}

// Declare 声明"本任务应当产出什么"。expect=-1 表示"至少 1 个"。
func (c *RunCtx) Declare(kind, name string, expect int) {
	if _, ok := c.artifacts[name]; !ok {
		c.order = append(c.order, name)
	}
	c.artifacts[name] = &Artifact{Kind: kind, Name: name, Expect: expect, Actual: 0}
}

// Actual 登记实际产出数量。未 Declare 就 Actual 视为编程错误（panic）。
func (c *RunCtx) Actual(name string, n int) {
	a, ok := c.artifacts[name]
	if !ok {
		panic(fmt.Sprintf("RunCtx.Actual: 未 Declare 就 Actual(%q)，属编程错误", name))
	}
	a.Actual = n
}

// Degrade 唯一的跳过出口：记录降级/跳过原因，并将任务标记为 degraded。
func (c *RunCtx) Degrade(code, reason string) {
	c.degrades = append(c.degrades, Degradation{Code: code, Reason: reason})
	c.degraded = true
}

// Assert 由调度器在任务结束后调用：比对 Declare 与 Actual，
// 缺失项返回 ArtifactMiss 列表，并自动将任务标记为 degraded。
func (c *RunCtx) Assert() []ArtifactMiss {
	var misses []ArtifactMiss
	names := append([]string(nil), c.order...)
	sort.Strings(names) // 稳定顺序
	for _, name := range names {
		a := c.artifacts[name]
		if a == nil {
			continue
		}
		miss := a.Expect > 0 && a.Actual < a.Expect
		missAtLeastOne := a.Expect == -1 && a.Actual < 1
		if miss || missAtLeastOne {
			misses = append(misses, ArtifactMiss{
				Kind:   a.Kind,
				Name:   a.Name,
				Expect: a.Expect,
				Actual: a.Actual,
				Code:   ArtifactCode,
			})
			c.degraded = true
		}
	}
	return misses
}

// Degraded 任务是否处于降级态（调用过 Degrade 或 Assert 发现缺失）。
func (c *RunCtx) Degraded() bool {
	return c.degraded
}

// Degradations 返回降级/跳过记录。
func (c *RunCtx) Degradations() []Degradation {
	return append([]Degradation(nil), c.degrades...)
}

// Ctx 返回关联的 context。
func (c *RunCtx) Ctx() context.Context {
	return c.ctx
}

// JobName 返回任务名。
func (c *RunCtx) JobName() string { return c.jobName }

// TradeDate 返回交易日期。
func (c *RunCtx) TradeDate() string { return c.tradeDate }
