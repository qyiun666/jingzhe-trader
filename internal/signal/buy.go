package signal

import (
	"context"

	"jingzhe-trader/internal/model"
)

// BuyDecider 买入决策接口 —— 由 llm.Reviewer 实现。
//
// 用户拍板（2026-09-03 决策 1）：买什么、买多少由 LLM 定，风控只做硬截断。
// 因此本接口是买入链上唯一的"要不要买"，规则证据只是它的输入。
//
// 批量而非逐只（用户拍板 2026-09-03 第四轮）：筛选后的候选一次性交给模型。
// 逐只问会把 5 条 prompt 变成 5N 次调用，联网那一条还要把上下文重烧 N 遍。
type BuyDecider interface {
	// DecideBatch 对整批候选出一次裁决，返回按 ts_code 索引的结论。
	// 某个标的在 map 里缺失 = 没问出结果（按失败处理，不是"模型说不要买"）。
	DecideBatch(ctx context.Context, req BatchRequest) (map[string]BuyDecision, error)
	// Enabled 是否真的有决策者在跑。false 意味着当日不可能有买单，必须显式可见。
	Enabled() bool
}

// BarSeries 单只股票的指标输入（按日期升序）：Closes 前复权收盘（元）、
// Vols 成交量（手）、Raws 未复权收盘（分）。
// Raws 单独留着是为了算真实成交额（复权价乘出来的成交额在除权日会失真）。
type BarSeries struct {
	Closes []float64
	Vols   []float64
	Raws   []float64
}

// BatchRequest 一批买入评审的输入：当日全部候选（同一天只问一次）。
type BatchRequest struct {
	TradeDate string
	Items     []BuyRequest
}

// BuyRequest 单只候选的评审输入。
type BuyRequest struct {
	TradeDate string
	Candidate model.Candidate
	Bars      BarSeries
	Rules     RuleEvidence
	RulesOK   bool      // false = 窗口不足以算证据，必须把这句话原样告诉模型
	Budget    BuyBudget // 风控口径的钱与额度：模型给的权重会被它截断
}

// BuyBudget 本次决策可用的钱（分）。模型只在给定额度内表达意愿，越界由风控斩掉。
type BuyBudget struct {
	CashFen    model.Fen // 可用现金
	SlotFen    model.Fen // 单票上限（含单笔金额上限取严后的较小值）
	LotCostFen model.Fen // 一手成本
	Positions  int       // 当前持仓只数
	MaxPos     int       // 档位允许的最大持仓只数
}

// BuyDecision 模型的裁决。
//
// WeightPct 是"拟投入占总资产比例"（0~1），不是股数 —— 换成整手股数、扣现金、
// 卡单票上限是风控的活。置信度低于档位下限会被风控拒（见 risk.Manager）。
// 止损价不由模型给：日内扫描与指令单止损价一律按档位参数算，
// 模型放宽止损就等于让硬风控失效。
type BuyDecision struct {
	Approve    bool
	WeightPct  float64
	Confidence float64
	Reason     string
	Failed     bool // true = 评审没问出结果（调用/解析失败），不是"评审认为不该买"
}

// NoDecider 模型未启用时的决策器：一律不买。
//
// 存在的意义是把"没有决策者"变成显式事实，而不是偷偷退回规则信号那条老路。
type NoDecider struct{}

// DecideBatch 永远整批不批。
func (NoDecider) DecideBatch(_ context.Context, req BatchRequest) (map[string]BuyDecision, error) {
	out := make(map[string]BuyDecision, len(req.Items))
	for _, it := range req.Items {
		out[it.Candidate.TsCode] = BuyDecision{Reason: "llm.enabled=false，无决策者，当日不开新仓"}
	}
	return out, nil
}

// Enabled 恒为 false：这个实现的存在本身就是"没有决策者"的表达。
func (NoDecider) Enabled() bool { return false }
