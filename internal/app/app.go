// Package app 应用装配（Batch 2：挂接数据接入编排 + 实时行情源）。
//
// 依赖方向（ARCHITECTURE §1）：app 处于 L4 编排层，依赖 store/config 与业务编排包
// （dataloader）、适配层（tushare/quote）；不直接触网（网络只在适配层）。
package app

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/quote"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// Application 顶层应用容器。
type Application struct {
	store      *store.Store
	config     *config.Config
	tushare    *tushare.Client
	quote      quote.Source
	dataloader *dataloader.Dataloader
}

// Run 阻塞运行直到 ctx 取消（守护进程主循环；Batch 2 仅等待退出信号）。
func (a *Application) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Dataloader 返回数据接入器。
func (a *Application) Dataloader() *dataloader.Dataloader { return a.dataloader }

// Quote 返回实时行情源。
func (a *Application) Quote() quote.Source { return a.quote }

// RunTask 执行指定数据接入任务（jingzhectl run 入口）。
func (a *Application) RunTask(ctx context.Context, task, date string, limit, backDays int) error {
	switch task {
	case "calendar":
		return a.dataloader.SyncCalendar(ctx)
	case "daily":
		if date == "" {
			return fmt.Errorf("daily 任务需要 --date YYYYMMDD")
		}
		return a.dataloader.SyncDaily(ctx, date, backDays)
	case "fina":
		return a.dataloader.SyncFina(ctx, limit)
	default:
		return fmt.Errorf("未知任务: %s（支持 calendar/daily/fina）", task)
	}
}

// FreshnessCheck 运行数据新鲜度门禁（八检查，详见 dataloader.FreshnessGate）。
func (a *Application) FreshnessCheck(ctx context.Context, tradeDate string) (*dataloader.FreshnessReport, error) {
	return dataloader.NewFreshnessGate(a.store, a.config.GetInt("screen.min_bar_rows")).Check(ctx, tradeDate)
}
