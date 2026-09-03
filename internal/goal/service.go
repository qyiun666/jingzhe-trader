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

// Config 季度目标配置（来自 config goal.* 键）。
type Config struct {
	TargetPct         float64      // 季度目标收益率
	BudgetPct         float64      // 回撤预算
	Gear              GearConfig   // 状态机阈值
	Pace              PaceSettings // 落后策略
	InitialCapital    model.Fen    // 快照缺失时的基准回落值（分）
}

// DefaultConfig 默认配置（与 config/keys.go 默认值一致）。
func DefaultConfig() Config {
	return Config{
		TargetPct:      0.15,
		BudgetPct:      0.10,
		Gear:           DefaultGearConfig(),
		Pace:           PaceSettings{Policy: PolicyUnrestricted, MaxBoostPct: 0.10, BudgetBelow: 0.30},
		InitialCapital: model.FromFloat(10000),
	}
}

// Service 季度目标服务：三度量计算 + 档位状态机驱动 + 持久化 + 落后策略装配。
type Service struct {
	st        *store.Store
	cfg       Config
	now       func() time.Time
	raiseAlert AlertFunc // 可空
}

// NewService 构造目标服务。
func NewService(st *store.Store, cfg Config) *Service {
	return &Service{st: st, cfg: cfg, now: time.Now}
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
	ParamsJSON   string // 生效参数快照（goal_gear_log.params_snapshot）
}

// Evaluate 执行一次季度档位评估（§5.5）：
// 读状态 → 读快照（陈旧沿用并标注 stale_days）→ 三度量 → 状态机 → 持久化 → 装配落后策略。
func (s *Service) Evaluate(ctx context.Context, tradeDate string) (*Result, error) {
	gs, err := s.loadState(ctx, tradeDate)
	if err != nil {
		return nil, err
	}

	sn, staleDays, err := s.latestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	days, err := s.st.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取交易日列表失败: %w", err)
	}
	elapsed, total := market.QuarterTradeDays(days, tradeDate)

	quarter, _, _ := market.QuarterOf(tradeDate)
	quarterReset := gs.Quarter != quarter

	// 峰值只升不降（季内峰值从季初基准起算）
	newPeak := gs.PeakAsset
	if sn.TotalAsset > newPeak {
		newPeak = sn.TotalAsset
	}

	m := ComputeMetrics(gs.BaselineAsset, newPeak, sn.TotalAsset, s.cfg.TargetPct, s.cfg.BudgetPct, elapsed, total)
	m.Quarter = quarter
	m.StaleDays = staleDays

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
	newState.Quarter = quarter
	newState.QuarterStart, newState.QuarterEnd = quarterStartEnd(tradeDate)
	newState.PeakAsset = newPeak
	newState.CurrentGear = dec.To
	newState.ProfitLock = dec.ToLock
	newState.UpgradeStreak = dec.NewStreak
	newState.LastEvalDate = tradeDate
	newState.PacePolicy = s.cfg.Pace.Policy
	if quarterReset {
		// 季度重置：基准 = 当前快照（快照缺失回落本金），峰值同步重置，清除覆盖
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

	// 变更才写 goal_gear_log（含 params_snapshot，可回放"当时为什么给这条指令"）
	if dec.Changed {
		params := s.resolveParams(newState.CurrentGear, dec.ToLock, pace, sn.TotalAsset)
		b, _ := json.Marshal(params)
		res.ParamsJSON = string(b)
		log := model.GoalGearLog{
			TradeDate:      tradeDate,
			Quarter:        quarter,
			FromGear:       dec.From,
			ToGear:         dec.To,
			FromLock:       dec.FromLock,
			ToLock:         dec.ToLock,
			TriggerRule:    string(dec.Trigger),
			Progress:       m.Progress,
			BudgetConsumed: m.BudgetConsumed,
			PaceGap:        m.PaceGap,
			IsManual:       dec.IsManual,
			Reason:         dec.Reason,
			ParamsSnapshot: res.ParamsJSON,
			CreatedAt:      newState.UpdatedAt,
		}
		if err := s.st.GoalRepo().InsertGearLog(ctx, log); err != nil {
			return nil, err
		}
		observability.S().Infow("档位状态变更",
			"date", tradeDate, "from", string(dec.From), "to", string(dec.To),
			"trigger", string(dec.Trigger), "manual", dec.IsManual, "reason", dec.Reason)
	}
	return res, nil
}

// Brief 生成邮件顶部"目标还差多少"数据（notify.GoalBrief），供调度器日报/指令邮件渲染。
// 任何读取失败都回落零值（不阻断发信）；度量复用 ComputeMetrics 纯函数。
func (s *Service) Brief(ctx context.Context, tradeDate string) (notify.GoalBrief, error) {
	gs, err := s.loadState(ctx, tradeDate)
	if err != nil {
		return notify.GoalBrief{}, err
	}
	sn, _, err := s.latestSnapshot(ctx)
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
		Gear:         string(gs.CurrentGear),
		GearLabel:    gs.CurrentGear.Label(),
		ProfitLock:   gs.ProfitLock,
		ProgressPct:  m.Progress * 100,
		TargetPct:    m.TargetPct * 100,
		PaceGapPct:   m.PaceGap * 100,
		CashYuan:     float64(sn.Cash) / 100,
		TotalYuan:    float64(sn.TotalAsset) / 100,
	}, nil
}

// SetGear 人工覆盖档位（jingzhectl run gear <G1|G2|G3> --reason）。
// 覆盖解除锁利；untilDate 为空默认当日。写 goal_gear_log(is_manual=1) + action_log。
func (s *Service) SetGear(ctx context.Context, gear model.Gear, reason, untilDate, actor string) (*Result, error) {
	if !gear.Valid() {
		return nil, fmt.Errorf("非法档位: %q（应为 G1/G2/G3）", gear)
	}
	if reason == "" {
		return nil, fmt.Errorf("人工覆盖必须给出 --reason（拒绝无理由改档）")
	}
	gs, err := s.loadState(ctx, s.today())
	if err != nil {
		return nil, err
	}
	if untilDate == "" {
		untilDate = s.today()
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
	log := model.GoalGearLog{
		TradeDate:   s.today(),
		Quarter:     gs.Quarter,
		FromGear:    prevGear,
		ToGear:      gear,
		FromLock:    prevLock,
		ToLock:      false,
		TriggerRule: string(TriggerManualOverride),
		IsManual:    true,
		Reason:      reason,
		CreatedAt:   nowStr,
	}
	if err := s.st.GoalRepo().InsertGearLog(ctx, log); err != nil {
		return nil, err
	}
	if err := s.st.OpsRepo().InsertActionLog(ctx, model.ActionLog{
		TradeDate:  s.today(),
		Actor:      actor,
		ObjectType: "goal_state",
		ObjectID:   "1",
		Action:     "set_gear",
		BeforeValue: string(prevGear),
		AfterValue:  string(gear),
		Reason:     reason,
		CreatedAt:  nowStr,
	}); err != nil {
		return nil, fmt.Errorf("写入人工改档审计日志失败: %w", err)
	}
	m := GoalMetrics{Quarter: gs.Quarter}
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
	return s.st.GoalRepo().UpsertGoalState(ctx, gs)
}

// RiskParams 组装当前生效风控参数（信号/快照任务用）：
// 基准 → 档位 → 锁利 → 落后策略 → 物理熔断（Resolve 唯一链路）。
func (s *Service) RiskParams(ctx context.Context, tradeDate string) (risk.RiskParams, model.Gear, error) {
	gs, err := s.loadState(ctx, tradeDate)
	if err != nil {
		return risk.RiskParams{}, model.GearG1, err
	}
	sn, _, err := s.latestSnapshot(ctx)
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
	base := risk.DefaultBase(sn.TotalAsset)
	return risk.Resolve(base, gs.CurrentGear, gs.ProfitLock, pace), gs.CurrentGear, nil
}

// resolveParams 计算生效参数快照（无 IO，供 goal_gear_log.params_snapshot）。
func (s *Service) resolveParams(gear model.Gear, lock bool, pace risk.PaceAdjust, totalAsset model.Fen) risk.RiskParams {
	return risk.Resolve(risk.DefaultBase(totalAsset), gear, lock, pace)
}

// loadState 读取档位状态；无记录时初始化（G1 / 季度字段 / 基准回落本金）。
func (s *Service) loadState(ctx context.Context, tradeDate string) (model.GoalState, error) {
	gs, err := s.st.GoalRepo().GetGoalState(ctx)
	if err == nil {
		if gs.PacePolicy == "" {
			gs.PacePolicy = s.cfg.Pace.Policy
		}
		return gs, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return gs, err
	}
	// 初始化单行（quarter 字段按评估日填充）
	quarter, qstart, qend := market.QuarterOf(tradeDate)
	base := s.cfg.InitialCapital
	if sn, _, serr := s.latestSnapshot(ctx); serr == nil && sn.TotalAsset > 0 {
		base = sn.TotalAsset
	}
	gs = model.GoalState{
		Quarter:      quarter,
		QuarterStart: qstart,
		QuarterEnd:   qend,
		BaselineAsset: base,
		PeakAsset:    base,
		CurrentGear:  model.GearG1,
		PacePolicy:   s.cfg.Pace.Policy,
		UpdatedAt:    s.now().UTC().Format(time.RFC3339),
	}
	if err := s.st.GoalRepo().UpsertGoalState(ctx, gs); err != nil {
		return model.GoalState{}, err
	}
	observability.S().Infow("初始化档位状态", "quarter", quarter, "baseline", int64(base))
	return gs, nil
}

// latestSnapshot 读取最近快照并计算陈旧交易日数（无快照回落本金，staleDays=-1 标注）。
func (s *Service) latestSnapshot(ctx context.Context) (model.AccountSnapshot, int, error) {
	sn, err := s.st.TradeRepo().LatestSnapshot(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.AccountSnapshot{
				Cash: s.cfg.InitialCapital, MarketValue: 0, TotalAsset: s.cfg.InitialCapital,
			}, -1, nil
		}
		return sn, 0, fmt.Errorf("读取账户快照失败: %w", err)
	}
	stale := 0
	if days, derr := s.st.MarketRepo().TradeDateList(ctx); derr == nil {
		today := s.today()
		for _, d := range days {
			if d > sn.TradeDate && d <= today {
				stale++
			}
		}
	}
	return sn, stale, nil
}

func (s *Service) today() string {
	return s.now().In(market.Loc).Format("20060102")
}

// quarterStartEnd 季度边界（YYYY-MM-DD）。
func quarterStartEnd(date string) (string, string) {
	_, start, end := market.QuarterOf(date)
	return start, end
}
