// Package mcp 对外 MCP（Model Context Protocol）接口：供外部 AI agent 读取系统状态、
// 回执成交、触发当日流程。所有写操作均落 action_log 以备审计（§10.6-6）。
package mcp

import (
	"context"

	"jingzhe-trader/internal/config"
	"jingzhe-trader/internal/dataloader"
	"jingzhe-trader/internal/goal"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/signal"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// Deps 为 MCP 工具提供的服务集合（由 BuildDeps 装配）。
type Deps struct {
	Store      *store.Store
	Config     *config.Config
	Screen     *screener.Screener
	Signal     *signal.Service
	Ledger     *ticket.Ledger
	Goal       *goal.Service
	Freshness  *dataloader.FreshnessGate
	MinBarRows int
}

// BuildDeps 由 store + config 装配全部下游服务（与 jingzhectl run task 同源装配）。
func BuildDeps(ctx context.Context, st *store.Store, cfg *config.Config) (*Deps, error) {
	ic := initialCapitalOf(cfg)
	deps := &Deps{
		Store:      st,
		Config:     cfg,
		Screen:     screener.New(st, filterConfigFrom(cfg)),
		Signal:     signal.NewService(st, ic, costParamsFrom(cfg)),
		Ledger:     ticket.NewLedger(st, costParamsFrom(cfg), ic),
		Goal:       goal.NewService(st, goalConfigOf(cfg)),
		Freshness:  dataloader.NewFreshnessGate(st, cfg.GetInt("screen.min_bar_rows")),
		MinBarRows: cfg.GetInt("screen.min_bar_rows"),
	}
	return deps, nil
}

// filterConfigFrom 从配置构建粗筛参数。
func filterConfigFrom(cfg *config.Config) screener.FilterConfig {
	fc := screener.DefaultFilterConfig()
	if v := cfg.GetInt("screen.top_n"); v > 0 {
		fc.TopN = v
	}
	if v := cfg.GetFloat("screen.min_circ_mv_w"); v > 0 {
		fc.MinCircMvW = v
	}
	if v := cfg.GetFloat("screen.min_turnover_rate"); v > 0 {
		fc.MinTurnoverRate = v
	}
	if v := cfg.GetFloat("screen.price_low"); v > 0 {
		fc.PriceLow = v
	}
	if v := cfg.GetFloat("screen.price_high"); v > 0 {
		fc.PriceHigh = v
	}
	if v := cfg.GetFloat("screen.pe_ttm_max"); v > 0 {
		fc.PETtmMax = v
	}
	if v := cfg.GetFloat("screen.pb_max"); v > 0 {
		fc.PBMax = v
	}
	return fc
}

// riskParamsFrom 组装生效风控参数：档位/锁利取自 goal_state，落后策略取自 config。
func riskParamsFrom(ctx context.Context, cfg *config.Config, st *store.Store) (risk.RiskParams, model.Gear) {
	gear := model.GearG1
	lock := false
	if gs, err := st.GoalRepo().GetGoalState(ctx); err == nil && gs.CurrentGear.Valid() {
		gear = gs.CurrentGear
		lock = gs.ProfitLock
	}
	totalAsset := initialCapitalOf(cfg)
	if sn, err := st.TradeRepo().LatestSnapshot(ctx); err == nil && sn.TotalAsset > 0 {
		totalAsset = sn.TotalAsset
	}
	base := risk.DefaultBase(totalAsset)
	if v := cfg.GetFloat("risk.max_sector_pct"); v > 0 {
		base.MaxSectorPct = v
	}
	if v := cfg.GetFloat("risk.take_profit_pct"); v > 0 {
		base.TakeProfitPct = v
	}
	var pace risk.PaceAdjust = risk.NoPace{}
	switch cfg.GetString("goal.pace_policy") {
	case "unrestricted":
		pace = risk.UnrestrictedPace{}
	case "conservative":
		pace = risk.ConservativePace{}
	}
	return risk.Resolve(base, gear, lock, pace), gear
}

// initialCapitalOf 本金（config account.initial_capital，单位元）；未配置按 1 万元回落。
func initialCapitalOf(cfg *config.Config) model.Fen {
	if v := cfg.GetFloat("account.initial_capital"); v > 0 {
		return model.FromFloat(v)
	}
	return model.FromFloat(10000)
}

// costParamsFrom 交易成本参数（config cost.* 键）。
func costParamsFrom(cfg *config.Config) market.CostParams {
	return market.CostParams{
		CommissionRate:  cfg.GetFloat("cost.commission_rate"),
		MinCommission:   model.FromFloat(cfg.GetFloat("cost.min_commission")),
		StampTaxRate:    cfg.GetFloat("cost.stamp_tax_rate"),
		TransferFeeRate: cfg.GetFloat("cost.transfer_fee_rate"),
	}
}

// goalConfigOf 由全局 config 拼装目标域配置。
func goalConfigOf(cfg *config.Config) goal.Config {
	gc := goal.DefaultConfig()
	if v := cfg.GetFloat("goal.target_pct"); v > 0 {
		gc.TargetPct = v
	}
	if v := cfg.GetFloat("goal.budget_pct"); v > 0 {
		gc.BudgetPct = v
	}
	ic := initialCapitalOf(cfg)
	if ic > 0 {
		gc.InitialCapital = ic
	}
	switch cfg.GetString("goal.pace_policy") {
	case "unrestricted", "conservative":
		gc.Pace.Policy = cfg.GetString("goal.pace_policy")
	}
	return gc
}
