package signal

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/internal/screener"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/ticket"
)

// Service 决策编排：候选 + 持仓 → 买入决策（LLM）/卖出决策（规则）→ 批次风控截断 → 指令单。
//
// 信号不落库：一条流水线内内存传递，唯一落库的结果是 order_ticket；
// 重跑幂等由"当日同标的同方向活跃指令单"集合保证（配 order_ticket 部分唯一索引）。
type Service struct {
	st     *store.Store
	ledger *ticket.Ledger // 现金/资产的唯一口径（含组合同步的现金锚点）
	gate   store.MarketGate
	etf    ETFUniverse // 白名单认定的场内 ETF（豁免规则 4 + 显示名兜底）
}

// ETFUniverse 白名单声明的场内 ETF：ts_code → 人工复核过的显示名。
//
// 两个用途都关系到钱，不能靠代码前缀猜：
//  1. 卖出规则 4（排名淘汰）对它跳过 —— ETF 候选只在弱势试探期存在且不排名，
//     按个股口径"跌出榜单就卖"会在恢复开闸当晚无条件清掉 ETF 仓位。
//  2. 指令单显示名兜底 —— 名称原先只能从 stock_basic 取，取不到就是整链 error；
//     ETF 是白名单自带名字的补口品种，缺行不该让一张止损单发不出去。
type ETFUniverse map[string]string

// NewService 构造决策服务。gate 决定卖出规则 5（大盘恶化）是否参与判定、
// 用多深的均线——必须与买入漏斗吃的是同一份配置，否则会出现
// "买入放行了、持仓当晚被大盘恶化规则清光"这种左右互搏。
//
// etf 传 screen.etf_whitelist 的解析结果（与 ETF 漏斗共用同一份配置，见
// scheduler.ETFOptionsOf）。按白名单而不是按 etf_enabled 生效：关闸买入不等于
// 手里没有 ETF，试探期买到的仓位在停闸之后仍要按正确的规则退出。
func NewService(st *store.Store, ledger *ticket.Ledger, gate store.MarketGate, etf ETFUniverse) *Service {
	return &Service{st: st, ledger: ledger, gate: gate, etf: etf}
}

// isETF 判定代码是否由白名单声明为场内 ETF。
func (s *Service) isETF(code string) bool {
	_, ok := s.etf[code]
	return ok
}

// SellName 补卖出指令单的显示名：优先取 stock_basic，缺失时回落到白名单名称。
//
// 个股沿用原口径（取不到就是错误：说明数据链没跑，必须出声）；只对白名单内的
// ETF 回落，因为它的名字本来就是配置里人工复核过的一手信息，不存在"数据没到"
// 的问题。盘中 M3 与收盘决策共用这一个实现，两条退出路径的名称口径不会漂移。
func (s *Service) SellName(ctx context.Context, code string) (string, error) {
	nm, err := s.st.ScreenRepo().StockName(ctx, code)
	if err == nil {
		return nm, nil
	}
	if alt := s.etf[code]; alt != "" {
		observability.S().Infow("持仓名称取自 ETF 白名单（stock_basic 无此行）",
			"ts_code", code, "name", alt, "err", err.Error())
		return alt, nil
	}
	return "", err
}

// Rejection 风控否决记录（回显给调用方并写日志；禁静默丢弃，D1）。
type Rejection struct {
	TsCode string
	Rule   string
	Msg    string
}

// Report 一次决策生成的产出（供 CLI 打印，计数只写日志）。
type Report struct {
	TradeDate   string
	Candidates  int // 进入决策链的候选数
	Approved    int // 模型批准买入的候选数
	Declined    int // 模型判"不买"的候选数
	Failed      int // 评审未问出结果的候选数（调用/解析失败 —— 不是"不该买"，是"不知道"）
	SellSignals int // 卖出决策数
	Rejected    int // 被风控硬截断否决数
	Skipped     int // 当日已有同向活跃指令单而跳过（重跑幂等）
	Tickets     int // 新增指令单数
	Rejections  []Rejection
	Notes       []string
	Empty       bool // 候选池为空（选股阶段已告警，此处不重复）
}

// buyProposal 一条待落地买入：决策信号 + 模型要求的金额（风控负责截断成整手股数）。
type buyProposal struct {
	sig  model.Signal
	want model.Fen
}

// Generate 执行决策主流程：① 读行情窗口与持仓 → ② 卖出决策（规则）→ ③ 买入决策（LLM）→ ④ 写指令单。
func (s *Service) Generate(ctx context.Context, tradeDate string, cands []model.Candidate,
	p risk.RiskParams, decider BuyDecider) (*Report, error) {
	if decider == nil {
		decider = NoDecider{}
	}
	rep := &Report{TradeDate: tradeDate, Candidates: len(cands)}
	series, err := s.barSeries(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	positions, err := s.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return nil, err
	}
	cash, err := s.cashFen(ctx)
	if err != nil {
		return nil, err
	}
	state := accountStateOf(positions, cash)
	idx, err := s.indexState(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	sells, err := s.sellDecisions(ctx, tradeDate, cands, positions, p, idx, rep)
	if err != nil {
		return nil, err
	}
	buys, err := s.buyDecisions(ctx, tradeDate, cands, series, positions, state, p, decider, rep)
	if err != nil {
		return nil, err
	}
	if err := s.writeTickets(ctx, tradeDate, buys, sells, state, p, rep); err != nil {
		return nil, err
	}
	rep.Empty = len(cands) == 0 && rep.Tickets == 0
	return rep, nil
}

// sellDecisions 持仓 × 卖出规则：不受板块与候选门槛影响（跌出榜单也只能按规则卖）。
//
// 卖出刻意不走 LLM：止损止盈是硬风控，让模型参与"要不要执行止损"就是把风控交给它否决。
func (s *Service) sellDecisions(ctx context.Context, tradeDate string, cands []model.Candidate,
	positions []model.Position, p risk.RiskParams, idx indexInfo, rep *Report) ([]model.Signal, error) {
	inTopN := make(map[string]bool, len(cands))
	for _, c := range cands {
		inTopN[c.TsCode] = true
	}
	var out []model.Signal
	for _, pos := range positions {
		if pos.TotalQty <= 0 {
			continue
		}
		bar, err := s.st.ScreenRepo().LatestBarAt(ctx, pos.TsCode, tradeDate)
		if err != nil {
			return nil, fmt.Errorf("持仓 %s 卖出判定缺少收盘价数据: %w", pos.TsCode, err)
		}
		h := HoldingCtx{
			Pos: pos, LastClose: bar.RawClose, LastDate: bar.TradeDate,
			InTopN: inTopN[pos.TsCode], MarketBad: idx.bad(), IsETF: s.isETF(pos.TsCode),
		}
		if sig := EvalSell(tradeDate, h, p, idx.close, idx.ma60); sig != nil {
			nm, e := s.SellName(ctx, pos.TsCode)
			if e != nil {
				return nil, fmt.Errorf("卖出信号 %s 取名称失败: %w", pos.TsCode, e)
			}
			sig.Name = nm
			out = append(out, *sig)
			rep.SellSignals++
		}
	}
	return out, nil
}

// buyDecisions 候选批量 × 证据 → LLM 一次裁决整批。规则不再否决任何候选，只提供证据列。
//
// positions 用于把"该候选是否已持有"传给模型：已持有走加仓判断（Holding 非 nil），
// 未持有才是新仓。这也让风控的加仓路径有据可依（单票剩余额度）。
func (s *Service) buyDecisions(ctx context.Context, tradeDate string, cands []model.Candidate,
	series map[string]BarSeries, positions []model.Position, state risk.AccountState, p risk.RiskParams,
	decider BuyDecider, rep *Report) ([]buyProposal, error) {
	held := make(map[string]*model.Position, len(positions))
	for i := range positions {
		if positions[i].TotalQty <= 0 {
			continue
		}
		held[positions[i].TsCode] = &positions[i]
	}
	items := make([]BuyRequest, 0, len(cands))
	for _, c := range cands {
		bs := series[c.TsCode]
		ev, evOK := EvaluateRules(bs)
		items = append(items, BuyRequest{
			TradeDate: tradeDate, Candidate: c, Bars: bs, Rules: ev, RulesOK: evOK,
			Budget:  budgetOf(state, p, c.TsCode, c.Close),
			Holding: held[c.TsCode],
		})
	}
	if len(items) == 0 {
		return nil, nil
	}
	decisions, err := decider.DecideBatch(ctx, BatchRequest{TradeDate: tradeDate, Items: items})
	if err != nil {
		return nil, fmt.Errorf("批量买入决策失败: %w", err)
	}
	out := make([]buyProposal, 0, len(items))
	for _, it := range items {
		d, ok := decisions[it.Candidate.TsCode]
		if !ok { // 模型没给这个标的的结论：是"不知道"，不是"不该买"
			d = BuyDecision{Failed: true, Reason: "批量决策响应中缺少该标的的结论"}
		}
		pr, skipNote, keep := s.pendingBuy(it, d, state.TotalAsset, rep)
		if skipNote != "" {
			rep.Notes = append(rep.Notes, skipNote)
		}
		if keep {
			out = append(out, pr)
		}
	}
	return out, nil
}

// pendingBuy 把一条裁决翻译成待落地买入；返回 (proposal, 附加说明, 是否入选)。
// 否决与失败都在这里计数：日志与 Report 必须分清"模型说不买"和"没问出来"。
func (s *Service) pendingBuy(it BuyRequest, d BuyDecision, total model.Fen, rep *Report) (buyProposal, string, bool) {
	code := it.Candidate.TsCode
	if !d.Approve {
		if d.Failed {
			rep.Failed++
			observability.S().Warnw("买入评审未问出结果，该候选当日不参与建仓",
				"date", it.TradeDate, "ts_code", code, "reason", d.Reason)
		} else {
			rep.Declined++
			observability.S().Infow("买入候选被评审否决", "date", it.TradeDate, "ts_code", code, "reason", d.Reason)
		}
		return buyProposal{}, "", false
	}
	if it.Candidate.Close <= 0 {
		rep.Declined++
		return buyProposal{}, fmt.Sprintf("%s 收盘价非法，无法折算股数", code), false
	}
	rep.Approved++
	return buyProposal{
		sig: model.Signal{
			TradeDate: it.TradeDate, TsCode: code, Name: it.Candidate.Name,
			Direction: model.DirBuy, Rule: DecideRuleName,
			Confidence: d.Confidence, RefPrice: it.Candidate.Close, Reason: d.Reason,
		},
		want: model.FromFloat(total.Float() * clampWeight(d.WeightPct)),
	}, "", true
}

// budgetOf 把风控口径翻译成模型能读懂的几个钱数：能动用的现金、单票上限、一手成本、
// 还剩几个持仓名额，以及（若已持有该代码）现有敞口与还能加多少。
// 模型只在这些数之内表达意愿，越界由 Manager 斩掉。
func budgetOf(state risk.AccountState, p risk.RiskParams, code string, price model.Fen) BuyBudget {
	b := BuyBudget{
		CashFen: state.Cash, SlotFen: p.SingleCapFen(),
		LotCostFen: price.Mul(model.LotShares),
		Positions:  state.PositionCount, MaxPos: p.MaxPositions,
	}
	if held, ok := state.HeldMV[code]; ok {
		room := p.SingleCapFen() - held
		if room < 0 {
			room = 0
		}
		b.ExistingFen = held
		b.AddRoomFen = room
	}
	return b
}

// clampWeight 模型给的仓位比例收口到 [0,1]：越界是模型输出异常，不做解释性放大。
func clampWeight(w float64) float64 {
	if !(w > 0) { // 同时挡住 NaN：模型给出非法数字时按"不给仓位"处理
		return 0
	}
	if w > 1 {
		return 1
	}
	return w
}

// DecideRuleName 买入决策来源名（落 order_ticket.source）。
const DecideRuleName = "llm_review"

// writeTickets 决策落地为指令单：买入受硬截断，卖出按可卖量。
func (s *Service) writeTickets(ctx context.Context, tradeDate string, buys []buyProposal, sells []model.Signal,
	state risk.AccountState, p risk.RiskParams, rep *Report) error {
	existing, err := s.activeKeys(ctx, tradeDate)
	if err != nil {
		return err
	}
	days, err := s.st.MarketRepo().TradeDateList(ctx)
	if err != nil {
		return err
	}
	svc := ticket.NewService(s.st)
	if err := s.writeBuys(ctx, svc, buys, existing, state, p, days, rep); err != nil {
		return err
	}
	return s.writeSells(ctx, svc, sells, existing, p, days, rep)
}

// writeBuys 买入决策 → 硬风控截断 → 指令单（否决全部记录，禁静默丢弃）。
func (s *Service) writeBuys(ctx context.Context, svc *ticket.Service, buys []buyProposal,
	existing map[ticketKey]bool, state risk.AccountState, p risk.RiskParams,
	days []string, rep *Report) error {
	if len(buys) == 0 {
		return nil
	}
	intents := make([]risk.BuyIntent, 0, len(buys))
	for _, pr := range buys {
		intents = append(intents, risk.BuyIntent{
			TsCode: pr.sig.TsCode, Name: pr.sig.Name, RefPrice: pr.sig.RefPrice,
			Confidence: pr.sig.Confidence, WantFen: pr.want,
		})
	}
	decisions := risk.NewManager(p).CheckBatch(intents, state)
	for i, dec := range decisions {
		sig := buys[i].sig
		if !dec.Approved {
			rep.Rejected++
			rep.Rejections = append(rep.Rejections, Rejection{TsCode: dec.TsCode, Rule: dec.RejectRule, Msg: dec.RejectMsg})
			observability.S().Infow("买入被风控截断", "date", sig.TradeDate, "ts_code", dec.TsCode,
				"rule", dec.RejectRule, "msg", dec.RejectMsg)
			continue
		}
		if _, err := s.createOnce(ctx, svc, sig, dec.Qty, existing, p, days, rep); err != nil {
			return err
		}
	}
	return nil
}

// writeSells 卖出决策 → 指令单（数量 = 可卖量，T+1 当日买入不可卖）。
func (s *Service) writeSells(ctx context.Context, svc *ticket.Service, sells []model.Signal,
	existing map[ticketKey]bool, p risk.RiskParams, days []string, rep *Report) error {
	for _, sig := range sells {
		pos, err := s.st.TradeRepo().GetPosition(ctx, sig.TsCode)
		if err != nil {
			return fmt.Errorf("读取持仓 %s 失败: %w", sig.TsCode, err)
		}
		if pos.Available() <= 0 {
			continue // 当日买入不可卖：决策保留到次日
		}
		if _, err := s.createOnce(ctx, svc, sig, pos.Available(), existing, p, days, rep); err != nil {
			return err
		}
	}
	return nil
}

// createOnce 写一张指令单；当日已有同标的同方向活跃单则跳过（重跑幂等）。
func (s *Service) createOnce(ctx context.Context, svc *ticket.Service, sig model.Signal, qty model.Qty,
	existing map[ticketKey]bool, p risk.RiskParams, days []string, rep *Report) (bool, error) {
	k := ticketKey{sig.TsCode, sig.Direction}
	if existing[k] {
		rep.Skipped++
		return false, nil
	}
	if _, err := svc.Create(ctx, sig, qty, days); err != nil {
		return false, fmt.Errorf("生成 %s %s 指令单失败: %w", sig.Direction, sig.TsCode, err)
	}
	existing[k] = true
	rep.Tickets++
	return true, nil
}

type ticketKey struct {
	code      string
	direction model.Direction
}

func (s *Service) activeKeys(ctx context.Context, tradeDate string) (map[ticketKey]bool, error) {
	tks, err := s.st.TradeRepo().ListTickets(ctx, tradeDate, "")
	if err != nil {
		return nil, fmt.Errorf("读取当日指令单失败: %w", err)
	}
	out := make(map[ticketKey]bool, len(tks))
	for _, t := range tks {
		if t.Status == model.TicketDrafted || t.Status == model.TicketIssued {
			out[ticketKey{t.TsCode, t.Direction}] = true
		}
	}
	return out, nil
}

// barSeries 因子窗口内的收盘、成交量与未复权收盘序列（按代码）。
func (s *Service) barSeries(ctx context.Context, tradeDate string) (map[string]BarSeries, error) {
	dates, err := s.st.ScreenRepo().WindowDates(ctx, tradeDate, screener.BarWindow())
	if err != nil {
		return nil, err
	}
	pts, err := s.st.ScreenRepo().BarCloseSeries(ctx, dates)
	if err != nil {
		return nil, err
	}
	out := make(map[string]BarSeries, len(dates))
	for _, p := range pts {
		bs := out[p.TsCode]
		bs.Closes = append(bs.Closes, p.Close)
		bs.Vols = append(bs.Vols, p.VolLot)
		bs.Raws = append(bs.Raws, p.RawClose)
		out[p.TsCode] = bs
	}
	return out, nil
}

// indexInfo 大盘指数状态（收盘与均线同为分；均线按门槛窗口由读取层现算）。
type indexInfo struct {
	close model.Fen
	ma60  model.Fen // 门槛均线（字段名沿用历史；窗口可配置后实际为 MA{gate.Window}）
}

// bad 大盘恶化判定：指数收盘跌破门槛均线。
func (i indexInfo) bad() bool { return i.close < i.ma60 }

// indexState 读大盘指数。门槛关闭时不读：返回零值（bad()=false，规则 5 自然停用），
// 判据不参与判定时把"指数数据不可读"升级成整链失败，等于修一个停摆换来另一个停摆。
// 门槛开启时读不到、或均线凑不满窗口根数，都是错误：
// 拿不到基准时把"大盘恶化"判成"没恶化"，等于这条卖出规则今天没跑却没人知道。
func (s *Service) indexState(ctx context.Context, tradeDate string) (indexInfo, error) {
	if !s.gate.Enabled {
		return indexInfo{}, nil
	}
	idx, err := s.st.ScreenRepo().LatestMarketIndex(ctx, tradeDate, s.gate.Window)
	if err != nil {
		return indexInfo{}, err
	}
	if idx.MA <= 0 {
		return indexInfo{}, fmt.Errorf("大盘指数 %s 在 %s 前不足 %d 根日线，MA%d 不可算",
			store.MarketIndex, tradeDate, s.gate.Window, s.gate.Window)
	}
	return indexInfo{close: idx.Close, ma60: idx.MA}, nil
}

// cashFen 可用现金：唯一实现在 ticket.Ledger（本金/现金锚点 + 成交历史推算），
// 决策侧只调用不重算 —— 否则锚点口径会在两处漂移。
func (s *Service) cashFen(ctx context.Context) (model.Fen, error) {
	return s.ledger.Cash(ctx)
}

// accountStateOf 批次风控的账户状态快照（纯函数；市值按成本口径，保守值）。
//
// 只算 total_qty>0 的行：清仓后的持仓行不会消失，按行数算会把已卖光的票
// 既算进持仓只数（占 MaxPositions 名额）又算进 HeldCodes（挡住再买）。
func accountStateOf(positions []model.Position, cash model.Fen) risk.AccountState {
	state := risk.AccountState{Cash: cash}
	state.HeldCodes = make(map[string]bool, len(positions))
	state.HeldMV = make(map[string]model.Fen, len(positions))
	for _, pos := range positions {
		if pos.TotalQty <= 0 {
			continue
		}
		mv := pos.CostPrice.Mul(pos.TotalQty)
		state.PositionCount++
		state.PositionsMV += mv
		state.HeldCodes[pos.TsCode] = true
		state.HeldMV[pos.TsCode] = mv
	}
	state.TotalAsset = state.Cash + state.PositionsMV
	return state
}
