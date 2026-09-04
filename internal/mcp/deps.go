// Package mcp 对外 MCP（Model Context Protocol）接口：供外部 AI agent 读取系统状态、
// 回执成交、校准账本、作废指令单、触发当日流程。写动作的结果落在被改动的表上，当日成败汇总见 run_trace。
package mcp

import (
	"context"
	"time"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// Liveness 调度器存活状态（/healthz 用）。由组合根注入调度器实例，可为 nil。
type Liveness interface {
	IsRunning() bool
	LastTickAt() time.Time
}

// JobRunner 调度器的即时补跑入口（由 *scheduler.Scheduler 实现）。
type JobRunner interface {
	RunNamed(ctx context.Context, name, date, trigger string) error
	JobNames() []string
}

// Deps 本包声明自己需要的服务集合，由组合根 internal/app 装配注入。
//
// 这里禁止出现任何构造：MCP 与调度器持有同一批实例，
// agent 读到的目标/风控/账本才会与系统实际算出来的完全一致。
type Deps struct {
	Store      *store.Store
	Config     *config.Config
	Ledger     *ticket.Ledger
	Tickets    *ticket.Service
	Goal       *goal.Service
	Freshness  *dataloader.FreshnessGate
	Liveness   Liveness
	Jobs       JobRunner
	MinBarRows int
}
