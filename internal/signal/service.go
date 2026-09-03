package signal

import (
	"context"
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// Service 信号生成编排：买入规则 + 卖出规则 + 终审 + 批次风控 + 幂等落库 + 指令单生成。
type Service struct {
	st             *store.Store
	initialCapital model.Fen // 快照缺失时的总资产回落值（分）
	cost           market.CostParams
}

// NewService 构造信号服务。
func NewService(st *store.Store, initialCapital model.Fen, cost market.CostParams) *Service {
	return &Service{st: st, initialCapital: initialCapital, cost: cost}
}

// Rejection 风控否决记录（回显给调用方；同时已落库 signal.reject_rule/reject_msg）。
type Rejection struct {
	TsCode string
	Rule   string
	Msg    string
}

// Report 一次信号生成的产出（供 CLI 打印与任务记录）。
type Report struct {
	TradeDate  string
	Candidates int // 当日候选池容量
	BuySignals int // 触发买入规则的信号数（终审后）
	SellSignals int // 触发卖出规则的信号数
	Inserted   int // 本次实际新增落库行数（重跑为 0，验收 #4）
	Rejected   int // 被批次风控否决的信号数（100% 落库）
	Tickets    int // 生成的指令单数
	Rejections []Rejection
	Notes      []string // 降级/提示（如指数数据缺失）
	Empty      bool     // 候选池为空（选股阶段已告警，此处不再重复）
}

// Generate 执行信号生成主流程。
//
// 幂等契约：signal 表有 (trade_date, ts_code, direction, rule) 唯一索引，
// 连续两次运行第二次不增行（验收 #4）。
// 风控否决 100% 落库：每条被拒信号都写 reject_rule + reject_msg（验收 #8）。
func (s *Service) Generate(ctx context.Context, tradeDate string, p risk.RiskParams, gear model.Gear, confirmer BuyConfirmer) (*Report, error) {
	rep := &Report{TradeDate: tradeDate}
	if confirmer == nil {
		confirmer = PassThroughConfirmer{}
	}

	// ---------- 数据装载 ----------
	cands, err := s.st.ScreenRepo().ListScreenResults(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	rep.Candidates = len(cands)
	if len(cands) == 0 {
		rep.Empty = true
		return rep, nil // 选股阶段已落 SCREEN_EMPTY 告警与观察名单
	}
	nameMap, err := s.st.ScreenRepo().StockNameMap(ctx)
	if err != nil {
		return nil, err
	}
	watch, err := s.st.ScreenRepo().ListWatchlist(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	inWatch := make(map[string]bool, len(watch))
	for _, w := range watch {
		inWatch[w.TsCode] = true
	}
	positions, err := s.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(positions))
	for _, pos := range positions {
		if pos.TotalQty > 0 {
			held[pos.TsCode] = true
		}
	}

	// 日线序列（指标计算）
	dates, err := s.st.ScreenRepo().RecentTradeDates(ctx, tradeDate, minBars)
	if err != nil {
		return nil, err
	}
	seriesByCode := map[string]BarSeries{}
	if len(dates) > 0 {
		points, err := s.st.ScreenRepo().BarCloseSeries(ctx, dates[len(dates)-1])
		if err != nil {
			return nil, err
		}
		for _, pt := range points {
			bs := seriesByCode[pt.TsCode]
			bs.Closes = append(bs.Closes, pt.Close)
			bs.Vols = append(bs.Vols, pt.VolLot)
			seriesByCode[pt.TsCode] = bs
		}
	}

	// 大盘状态（指数数据缺失 → MarketBad=false 并显式记录，不静默）
	indexClose, indexMA20 := model.Fen(0), 0.0
	idx, err := s.st.ScreenRepo().LatestIndexAny(ctx, tradeDate)
	if err != nil {
		rep.Notes = append(rep.Notes, "指数日线缺失，大盘恶化规则本轮不判定")
	} else {
		indexClose, indexMA20 = idx.Close, idx.MA20
	}

	// ---------- 卖出信号（持仓 × 五规则） ----------
	candSet := make(map[string]bool, len(cands))
	for _, c := range cands {
		candSet[c.TsCode] = true
	}
	var signals []model.Signal
	for _, pos := range positions {
		if pos.TotalQty <= 0 {
			continue
		}
		bar, err := s.st.ScreenRepo().LatestBarAt(ctx, pos.TsCode, tradeDate)
		if err != nil {
			return nil, fmt.Errorf("读取持仓 %s 最近收盘失败: %w", pos.TsCode, err)
		}
		h := HoldingCtx{
			Pos:       pos,
			LastClose: bar.RawClose, // 未复权收盘：与成交成本同口径
			LastDate:  bar.TradeDate,
			InTopN:    candSet[pos.TsCode],
			InWatch:   inWatch[pos.TsCode],
			MarketBad: indexMA20 > 0 && indexClose < model.Fen(indexMA20),
		}
		sig := EvalSell(tradeDate, h, p, indexClose, indexMA20)
		if sig != nil {
			if nm, ok := nameMap[pos.TsCode]; ok {
				sig.Name = nm
			}
			signals = append(signals, *sig)
			rep.SellSignals++
		}
	}

	// ---------- 买入信号（候选 × 趋势确认 × LLM 终审） ----------
	scoreByCode := make(map[string]float64, len(cands))
	for _, c := range cands {
		scoreByCode[c.TsCode] = c.Score
		bc := BuyCandidate{Result: c, Name: nameMap[c.TsCode]}
		sig := EvalBuy(bc, seriesByCode[c.TsCode], p)
		if sig == nil {
			continue
		}
		ok, err := confirmer.Confirm(ctx, bc)
		if err != nil {
			return nil, fmt.Errorf("买入终审 %s 失败: %w", c.TsCode, err)
		}
		if !ok {
			continue // 终审否决：不产生信号（否决原因由终审实现方落 llm_call，Batch 4）
		}
		signals = append(signals, *sig)
		rep.BuySignals++
	}

	// ---------- 幂等落库（INSERT OR IGNORE + 唯一索引） ----------
	before, err := s.st.DecisionRepo().CountSignals(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range signals {
		signals[i].CreatedAt = now
		if err := s.st.DecisionRepo().InsertSignal(ctx, signals[i]); err != nil {
			return nil, err
		}
	}
	after, err := s.st.DecisionRepo().CountSignals(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	rep.Inserted = after - before

	// ---------- 批次风控（仅买入；卖出不受仓位/资金约束） ----------
	days, err := s.st.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return nil, err
	}
	tkSvc := ticket.NewService(s.st)
	var intents []risk.BuyIntent
	var buySigs []model.Signal
	for _, sig := range signals {
		if sig.Direction != model.DirBuy {
			continue
		}
		intents = append(intents, risk.BuyIntent{
			TsCode:     sig.TsCode,
			Name:       sig.Name,
			RefPrice:   sig.RefPrice,
			Confidence: sig.Confidence,
			Score:      scoreByCode[sig.TsCode],
		})
		buySigs = append(buySigs, sig)
	}
	if len(intents) > 0 {
		state, err := s.accountState(ctx)
		if err != nil {
			return nil, err
		}
		decisions := risk.NewManager(p).CheckBatch(intents, state)
		for i, dec := range decisions {
			sigID, err := s.st.DecisionRepo().FindSignalID(ctx, tradeDate, dec.TsCode, model.DirBuy, BuyRuleName)
			if err != nil {
				return nil, err
			}
			if dec.Approved {
				// 生成买入指令单（drafted，等待人工确认后 issued）
				if _, err := tkSvc.Create(ctx, buySigs[i], dec.Qty, p, gear, days); err != nil {
					return nil, err
				}
				rep.Tickets++
				continue
			}
			// 否决 100% 落库（验收 #8）：禁静默 continue
			if err := s.st.DecisionRepo().MarkRejected(ctx, sigID, dec.RejectRule, dec.RejectMsg); err != nil {
				return nil, err
			}
			rep.Rejected++
			rep.Rejections = append(rep.Rejections, Rejection{TsCode: dec.TsCode, Rule: dec.RejectRule, Msg: dec.RejectMsg})
		}
	}

	// ---------- 卖出指令单 ----------
	for _, sig := range signals {
		if sig.Direction != model.DirSell {
			continue
		}
		pos, err := s.st.TradeRepo().GetPosition(ctx, sig.TsCode)
		if err != nil {
			return nil, err
		}
		qty := pos.AvailableQty // T+1：只卖可卖部分
		if qty <= 0 {
			continue // 可卖量为 0（当日买入）：不下单，信号保留供次日执行
		}
		if _, err := tkSvc.Create(ctx, sig, qty, p, gear, days); err != nil {
			return nil, err
		}
		rep.Tickets++
	}

	return rep, nil
}

// accountState 构建批次风控的账户状态：总资产取最新快照，缺省回落本金；
// 持仓市值按成本口径（风控核算用保守值）。
func (s *Service) accountState(ctx context.Context) (risk.AccountState, error) {
	state := risk.AccountState{TotalAsset: s.initialCapital}
	if sn, err := s.st.TradeRepo().LatestSnapshot(ctx); err == nil {
		state.TotalAsset = sn.TotalAsset
	}
	positions, err := s.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return state, err
	}
	state.HeldCodes = map[string]bool{}
	for _, pos := range positions {
		if pos.TotalQty <= 0 {
			continue
		}
		state.HeldCodes[pos.TsCode] = true
		state.PositionCount++
		state.PositionsMV += pos.CostPrice.Mul(pos.TotalQty)
	}
	if state.PositionsMV > state.TotalAsset {
		state.PositionsMV = state.TotalAsset // 成本口径失真兜底
	}
	state.Cash = state.TotalAsset - state.PositionsMV
	return state, nil
}
