package goal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/notify"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
)

// AlertFunc 告警回调（由 notify.AlertService 注入，避免 goal 反向依赖 notify）。
type AlertFunc func(ctx context.Context, tradeDate, source string, level model.AlertLevel, code, title, content string) error

// AssetReader 当前账户资产来源（现金 / 市值 / 总资产）。由 ticket.Ledger 实现、组合根注入：
// 目标域只声明"我需要总资产"，不自己读交易域的表。
type AssetReader interface {
	Assets(ctx context.Context, tradeDate string) (model.Assets, error)
}

// Config 季度目标配置（来自 config goal.* 键）。
type Config struct {
	TargetPct     float64      // 季度目标收益率
	BudgetPct     float64      // 回撤预算
	Gear          GearConfig   // 状态机阈值
	Pace          PaceSettings // 落后策略
	MaxSectorPct  float64      // 板块集中度上限覆盖（config risk.max_sector_pct）
	TakeProfitPct float64      // 止盈线上限覆盖（config risk.take_profit_pct）
}

// DefaultConfig 默认配置（与 config/keys.go 默认值一致，测试夹具用；
// 生产配置由 app.GoalConfigOf 从 config_kv 读，不再两处各写一份）。
func DefaultConfig() Config {
	return Config{
		TargetPct:     0.15,
		BudgetPct:     0.10,
		Gear:          DefaultGearConfig(),
		Pace:          PaceSettings{Policy: PolicyUnrestricted, MaxBoostPct: 0.10, BudgetBelow: 0.30},
		MaxSectorPct:  0.50,
		TakeProfitPct: 0.15,
	}
}

// Service 季度目标服务：三度量计算 + 档位状态机驱动 + 持久化 + 落后策略装配。
type Service struct {
	st         *store.Store
	cfg        Config
	now        func() time.Time
	raiseAlert AlertFunc   // 可空
	assetsOf   AssetReader // 构造必填：档位度量的总资产只能由账本给出
}

// NewService 构造目标服务。assets 为当前账户资产来源（组合根传 *ticket.Ledger）；
// 它不是可选项：没有资产源就没有可信的总资产，而总资产是档位判定的分母。
func NewService(st *store.Store, cfg Config, assets AssetReader) *Service {
	return &Service{st: st, cfg: cfg, now: time.Now, assetsOf: assets}
}

// WithClock 注入时钟（测试用）。
func (s *Service) WithClock(f func() time.Time) *Service {
	if f != nil {
		s.now = f
	}
	return s
}

// WithAlertFunc 注入告警回调（PACE_BOOST_EXPIRED / PACE_BOOST_DENIED 显式告警）。
func (s *Service) WithAlertFunc(f AlertFunc) *Service {
	s.raiseAlert = f
	return s
}

// Result 一次评估的产出（供 CLI/调度器/日报消费）。
type Result struct {
	TradeDate    string
	Metrics      GoalMetrics
	Decision     Decision
	QuarterReset bool
	Pace         risk.PaceAdjust
	PaceCode     string // 激进加成被拒时的告警码（空 = 未被拒）
	PaceReason   string
	ParamsJSON   string // 生效参数快照（随返回值给调用方，变更过程写日志）
}

// Evaluate 执行一次季度档位评估（§5.5）：
// 读状态 → 读快照（陈旧沿用并标注 stale_days）→ 三度量 → 状态机 → 持久化 → 装配落后策略。
func (s *Service) Evaluate(ctx context.Context, tradeDate string) (*Result, error) {
	gs, err := s.loadState(ctx, tradeDate)
	if err != nil {
		return nil, err
	}

	m, sn, err := s.measure(ctx, gs, tradeDate)
	if err != nil {
		return nil, err
	}
	quarterReset := gs.Quarter != m.Quarter

	st := State{Gear: gs.CurrentGear, ProfitLock: gs.ProfitLock, UpgradeStreak: gs.UpgradeStreak}
	dec := Evaluate(st, m, s.cfg.Gear, EvalOptions{
		Today:         tradeDate,
		LastEvalDate:  gs.LastEvalDate,
		OverrideGear:  gs.OverrideGear,
		OverrideUntil: gs.OverrideUntil,
		QuarterReset:  quarterReset,
	})

	// 落后策略装配 + 激进模式显式拒绝告警
	pace := NewPaceAdjust(s.cfg.Pace, m, gs.PaceConfirmDate, tradeDate)
	res := &Result{TradeDate: tradeDate, Metrics: m, Decision: dec, QuarterReset: quarterReset, Pace: pace}
	if code, reason, denied := AggressiveDenied(s.cfg.Pace, m, gs.PaceConfirmDate, tradeDate); denied {
		res.PaceCode, res.PaceReason = code, reason
		if s.raiseAlert != nil {
			if aerr := s.raiseAlert(ctx, tradeDate, "goal", model.AlertWarning, code, "激进加成被拒/过期回落", reason); aerr != nil {
				return nil, fmt.Errorf("落激进加成告警失败: %w", aerr)
			}
		}
	}

	// 持久化新状态（streak 变化也落库，保证可回放）
	newState := gs
	newState.Quarter = m.Quarter
	newState.QuarterStart, newState.QuarterEnd = quarterStartEnd(tradeDate)
	newState.PeakAsset = m.PeakAsset
	newState.CurrentGear = dec.To
	newState.ProfitLock = dec.ToLock
	newState.UpgradeStreak = dec.NewStreak
	newState.LastEvalDate = tradeDate
	newState.PacePolicy = s.cfg.Pace.Policy
	if quarterReset {
		// 季度重置：基准与峰值都取当前总资产（资产读不到时 measure 已报错，不会拿本金凑），并清除覆盖
		newState.BaselineAsset = sn.TotalAsset
		newState.PeakAsset = sn.TotalAsset
		newState.OverrideGear, newState.OverrideReason, newState.OverrideUntil = "", "", ""
	}
	if dec.IsManual {
		newState.CurrentGear = dec.To
		newState.ProfitLock = false
	}
	// 覆盖过期：清除覆盖字段，回落自动评估
	if gs.OverrideGear != "" && gs.OverrideUntil != "" && gs.OverrideUntil < tradeDate {
		newState.OverrideGear, newState.OverrideReason, newState.OverrideUntil = "", "", ""
	}
	newState.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	if err := s.st.GoalRepo().UpsertGoalState(ctx, newState); err != nil {
		return nil, err
	}

	// 变更只记日志：档位的历史轨迹没有任何读者，当前档位就在 goal.state 里，
	// 生效参数快照随返回值给调用方（res.ParamsJSON）。
	observability.S().Infow("季度目标评估完成",
		"date", tradeDate, "quarter", m.Quarter, "gear", string(dec.To), "changed", dec.Changed,
		"progress", m.Progress, "target_pct", m.TargetPct, "budget_consumed", m.BudgetConsumed,
		"elapsed_days", m.ElapsedDays, "total_days", m.TotalDays, "reason", dec.Reason)
	if dec.Changed {
		params, err := s.resolveParams(newState.CurrentGear, dec.ToLock, pace, sn.TotalAsset)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("序列化生效风控参数失败: %w", err)
		}
		res.ParamsJSON = string(b)
		actor := "auto"
		if dec.IsManual {
			actor = "manual"
		}
		observability.S().Infow("档位状态变更",
			"date", tradeDate, "quarter", m.Quarter, "actor", actor,
			"from", string(dec.From), "from_lock", dec.FromLock,
			"to", string(dec.To), "to_lock", dec.ToLock,
			"trigger", string(dec.Trigger), "manual", dec.IsManual,
			"progress", m.Progress, "budget_consumed", m.BudgetConsumed, "pace_gap", m.PaceGap,
			"reason", dec.Reason, "params", res.ParamsJSON)
	}
	return res, nil
}

// Brief 生成邮件顶部"目标还差多少"数据（notify.GoalBrief），供调度器日报/指令邮件渲染。
// 任何读取失败都上抛错误：目标数字缺失的日报会被当成完整日报发出去，那比不发更糟。
// 度量复用 ComputeMetrics 纯函数。
func (s *Service) Brief(ctx context.Context, tradeDate string) (notify.GoalBrief, error) {
	gs, err := s.loadState(ctx, tradeDate)
	if err != nil {
		return notify.GoalBrief{}, err
	}
	sn, _, err := s.currentAssets(ctx)
	if err != nil {
		return notify.GoalBrief{}, err
	}
	days, err := s.st.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return notify.GoalBrief{}, fmt.Errorf("读取交易日列表失败: %w", err)
	}
	elapsed, total := market.QuarterTradeDays(days, tradeDate)
	newPeak := gs.PeakAsset
	if sn.TotalAsset > newPeak {
		newPeak = sn.TotalAsset
	}
	m := ComputeMetrics(gs.BaselineAsset, newPeak, sn.TotalAsset, s.cfg.TargetPct, s.cfg.BudgetPct, elapsed, total)
	return notify.GoalBrief{
		Gear:        string(gs.CurrentGear),
		GearLabel:   gs.CurrentGear.Label(),
		ProfitLock:  gs.ProfitLock,
		ProgressPct: m.Progress * 100,
		TargetPct:   m.TargetPct * 100,
		PaceGapPct:  m.PaceGap * 100,
		CashYuan:    float64(sn.Cash) / 100,
		TotalYuan:   float64(sn.TotalAsset) / 100,
	}, nil
}

// SetGear 人工覆盖档位（MCP 工具 set_gear）。
// 覆盖解除锁利；untilDate 为空默认当日。结果写在 goal.state 的 override_* 三个字段，变更只记日志。
func (s *Service) SetGear(ctx context.Context, gear model.Gear, reason, untilDate, actor string) (*Result, error) {
	if !gear.Valid() {
		return nil, fmt.Errorf("非法档位: %q（应为 G1/G2/G3）", gear)
	}
	if reason == "" {
		return nil, fmt.Errorf("人工覆盖必须给出 reason（拒绝无理由改档）")
	}
	gs, err := s.loadState(ctx, s.today())
	if err != nil {
		return nil, err
	}
	if untilDate == "" {
		untilDate = s.today()
	}
	// 先量再写：改档返回给外部 agent 的必须是与 Evaluate 同源的真实读数，
	// 拿不到读数就一行都不写，而不是写完再回一串零。
	m, _, err := s.measure(ctx, gs, s.today())
	if err != nil {
		return nil, err
	}
	nowStr := s.now().UTC().Format(time.RFC3339)
	prevGear := gs.CurrentGear
	prevLock := gs.ProfitLock
	gs.CurrentGear = gear
	gs.ProfitLock = false // 人工覆盖解除锁利（§8.2）
	gs.OverrideGear = string(gear)
	gs.OverrideReason = reason
	gs.OverrideUntil = untilDate
	gs.UpdatedAt = nowStr
	if err := s.st.GoalRepo().UpsertGoalState(ctx, gs); err != nil {
		return nil, err
	}
	// 改档结果本身已在 goal.state 的 override_gear / override_reason / override_until 里，
	// 这里只补记审计想要的「改档前值 + 操作者」——历史轨迹零读者，不落库。
	observability.S().Infow("人工改档",
		"date", s.today(), "actor", actor,
		"from", string(prevGear), "from_lock", prevLock,
		"to", string(gear), "until", untilDate, "reason", reason)
	return &Result{TradeDate: s.today(), Decision: Decision{
		From: prevGear, To: gear, Trigger: TriggerManualOverride, IsManual: true,
		Changed: true, Reason: reason,
	}, Metrics: m}, nil
}

// ConfirmPace 激进模式每日人工确认续期（三重保护③）。
func (s *Service) ConfirmPace(ctx context.Context, tradeDate string) error {
	gs, err := s.loadState(ctx, tradeDate)
	if err != nil {
		return err
	}
	gs.PaceConfirmDate = tradeDate
	gs.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	if err := s.st.GoalRepo().UpsertGoalState(ctx, gs); err != nil {
		observability.S().Errorw("激进节奏确认落库失败", "date", tradeDate, "err", err.Error())
		return err
	}
	observability.S().Infow("激进节奏确认已续期", "date", tradeDate, "policy", gs.PacePolicy)
	return nil
}

// measure 季度三度量：实时总资产 + 季内交易日数 + 峰值只升不降。
//
// Evaluate 与 SetGear 共用同一套读数：人工改档的回包若带一串零，
// 外部 agent 就会把"没算"读成"目标是 0、进度是 0"。
func (s *Service) measure(ctx context.Context, gs model.GoalState, tradeDate string) (GoalMetrics, model.Assets, error) {
	sn, staleDays, err := s.currentAssets(ctx)
	if err != nil {
		return GoalMetrics{}, sn, err
	}
	days, err := s.st.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return GoalMetrics{}, sn, fmt.Errorf("读取交易日列表失败: %w", err)
	}
	elapsed, total := market.QuarterTradeDays(days, tradeDate)
	quarter, _, _ := market.QuarterOf(tradeDate)
	newPeak := gs.PeakAsset
	if sn.TotalAsset > newPeak {
		newPeak = sn.TotalAsset // 季内峰值只升不降
	}
	m := ComputeMetrics(gs.BaselineAsset, newPeak, sn.TotalAsset, s.cfg.TargetPct, s.cfg.BudgetPct, elapsed, total)
	m.Quarter = quarter
	m.StaleDays = staleDays
	return m, sn, nil
}

// baseParams 风控基准 = risk.DefaultBase + config 覆盖项（板块集中度 / 止盈线）。
// RiskParams 与 resolveParams 共用，保证生效参数与落库参数快照同源。
func (s *Service) baseParams(totalAsset model.Fen) risk.RiskParams {
	base := risk.DefaultBase(totalAsset)
	if s.cfg.MaxSectorPct > 0 {
		base.MaxSectorPct = s.cfg.MaxSectorPct
	}
	if s.cfg.TakeProfitPct > 0 {
		base.TakeProfitPct = s.cfg.TakeProfitPct
	}
	return base
}

// RiskParams 组装当前生效风控参数（信号/快照任务用）：
// 基准 → 档位 → 锁利 → 落后策略 → 物理熔断（Resolve 唯一链路）。
func (s *Service) RiskParams(ctx context.Context, tradeDate string) (risk.RiskParams, model.Gear, error) {
	gs, err := s.loadState(ctx, tradeDate)
	if err != nil {
		return risk.RiskParams{}, model.GearG1, err
	}
	sn, _, err := s.currentAssets(ctx)
	if err != nil {
		return risk.RiskParams{}, model.GearG1, err
	}
	days, err := s.st.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return risk.RiskParams{}, model.GearG1, fmt.Errorf("读取交易日列表失败: %w", err)
	}
	elapsed, total := market.QuarterTradeDays(days, tradeDate)
	m := ComputeMetrics(gs.BaselineAsset, gs.PeakAsset, sn.TotalAsset, s.cfg.TargetPct, s.cfg.BudgetPct, elapsed, total)
	pace := NewPaceAdjust(s.cfg.Pace, m, gs.PaceConfirmDate, tradeDate)
	rp, err := risk.Resolve(s.baseParams(sn.TotalAsset), gs.CurrentGear, gs.ProfitLock, pace)
	if err != nil {
		return risk.RiskParams{}, gs.CurrentGear, err
	}
	return rp, gs.CurrentGear, nil
}

// resolveParams 计算生效参数快照（无 IO，供档位留痕）。
func (s *Service) resolveParams(gear model.Gear, lock bool, pace risk.PaceAdjust, totalAsset model.Fen) (risk.RiskParams, error) {
	return risk.Resolve(s.baseParams(totalAsset), gear, lock, pace)
}

// loadState 读取档位状态；无记录时按当日初始化（G1 + 季度字段 + 基准=当时总资产）。
func (s *Service) loadState(ctx context.Context, tradeDate string) (model.GoalState, error) {
	gs, err := s.st.GoalRepo().GetGoalState(ctx)
	if err == nil {
		if !gs.CurrentGear.Valid() {
			return gs, fmt.Errorf("档位状态里的 gear=%q 非法（可选 G1|G2|G3）", gs.CurrentGear)
		}
		if gs.PacePolicy == "" {
			return gs, fmt.Errorf("档位状态里的 pace_policy 为空：状态行须自带策略，不从配置回落")
		}
		return gs, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return gs, err
	}
	// 初始化单行（quarter 字段按评估日填充）
	quarter, qstart, qend := market.QuarterOf(tradeDate)
	sn, _, err := s.currentAssets(ctx)
	if err != nil {
		return model.GoalState{}, fmt.Errorf("初始化档位状态需要当前总资产: %w", err)
	}
	if sn.TotalAsset <= 0 {
		return model.GoalState{}, fmt.Errorf("初始化档位状态失败：总资产为 %d，先跑 jingzhe init", int64(sn.TotalAsset))
	}
	base := sn.TotalAsset
	gs = model.GoalState{
		Quarter:       quarter,
		QuarterStart:  qstart,
		QuarterEnd:    qend,
		BaselineAsset: base,
		PeakAsset:     base,
		CurrentGear:   model.GearG1,
		PacePolicy:    s.cfg.Pace.Policy,
		UpdatedAt:     s.now().UTC().Format(time.RFC3339),
	}
	if err := s.st.GoalRepo().UpsertGoalState(ctx, gs); err != nil {
		return model.GoalState{}, err
	}
	observability.S().Infow("初始化档位状态", "quarter", quarter, "baseline", int64(base))
	return gs, nil
}

// currentAssets 实时算出当前账户资产，并给出陈旧交易日数
// （市值取价截止日不是今天时，按交易日历数出落后几天）。未注入资产源直接报错。
func (s *Service) currentAssets(ctx context.Context) (model.Assets, int, error) {
	today := s.today()
	if s.assetsOf == nil {
		// 没有资产源就不出数：按本金顶上去会得到一个"看着正常但方向全错"的总资产，
		// 而它会直接进档位判定的分母。
		return model.Assets{}, 0, fmt.Errorf("goal 未注入资产源（NewService 第三参数），无法计算总资产")
	}
	a, err := s.assetsOf.Assets(ctx, today)
	if err != nil {
		return a, 0, err
	}
	if a.TradeDate == today {
		return a, 0, nil
	}
	stale := 0
	if days, derr := s.st.MarketRepo().TradeDateList(ctx); derr == nil {
		for _, d := range days {
			if d > a.TradeDate && d <= today {
				stale++
			}
		}
	}
	return a, stale, nil
}

func (s *Service) today() string {
	return s.now().In(market.Loc).Format("20060102")
}

// quarterStartEnd 季度边界（YYYY-MM-DD）。
func quarterStartEnd(date string) (string, string) {
	_, start, end := market.QuarterOf(date)
	return start, end
}
