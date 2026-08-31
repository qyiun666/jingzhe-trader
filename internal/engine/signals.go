package engine

// 统一信号汇集: 回测 Pipeline 与实盘计划生成共用同一套
// 合并/过滤/风控检查/排序语义, 防止两套实现漂移
//
// 约定:
//   - 止损/止盈信号优先, 策略对同一股票的信号剔除
//   - Advice 建议类信号 (如做T提示) 不进成交管道, 仅记日志
//   - 全部信号统一过风控检查, 拒绝原因返回给调用方展示
//   - 买入计划以批次为单位过可用资金核算 (先卖后买), 资金不足的缩量或拒因
//   - 结果按 "卖出在前" 排序 (先卖出释放资金再买入)

import (
	"fmt"
	"sort"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/risk"
	"jingzhe-trader/pkg/logger"
)

// MergeStrategySignals 合并止损信号与策略信号:
// 剔除与止损重复的标的与 Advice 建议类信号, 返回合并结果
// stopCodes 为已有止损信号的标的集合 (由调用方通过 CheckStopLoss 产生)
func MergeStrategySignals(date string, stopSignals []model.Signal, stopCodes map[string]bool,
	stratSignals []model.Signal) []model.Signal {

	merged := make([]model.Signal, 0, len(stopSignals)+len(stratSignals))
	merged = append(merged, stopSignals...)
	for _, s := range stratSignals {
		if s.Advice {
			logger.L().Infof("[%s] 建议信号(不执行) %s: %s", date, s.TsCode, s.Reason)
			continue
		}
		if !stopCodes[s.TsCode] {
			merged = append(merged, s)
		}
	}
	return merged
}

// RejectInfo 风控拒绝信息 (供调用方升级告警, 如止损被跌停拦截)
type RejectInfo struct {
	TsCode string `json:"ts_code"`
	Reason string `json:"reason"`
	Rule   string `json:"rule"`
}

// SignalInput 一轮信号汇集的输入 (回测与实盘共用)
type SignalInput struct {
	Date       string
	Risk       *risk.RiskManager
	Signals    []model.Signal
	Positions  map[string]*model.Position
	Stocks     map[string]*model.Stock
	Bars       map[string]*model.Bar
	TotalAsset float64
	Cash       float64           // 可用现金; <=0 表示不做资金约束 (退回仅仓位比例上限)
	Cost       *market.CostModel // 费用模型, nil 时按成交额毛额核算
}

// CheckAndSortSignals 统一过风控检查、按可用资金裁剪买入、并排序 (卖出在前)
// 返回: 通过信号; 拒绝列表用于展示/告警
func CheckAndSortSignals(in SignalInput) ([]model.Signal, []RejectInfo) {
	// 先定序再过风控: 持仓数上限与批次在途市值都是"逐笔占额"的裁定,
	// 顺序由策略遍历决定时, 哪两只票能建仓实际取决于 map 的遍历顺序
	ordered := make([]model.Signal, len(in.Signals))
	copy(ordered, in.Signals)
	sortSignalsByPriority(ordered)

	passed, rejections := in.Risk.Check(ordered, in.Positions, in.TotalAsset, in.Stocks, in.Date, in.Bars)
	funded, fundingRej := applyFunding(in, passed)
	rejections = append(rejections, fundingRej...)

	var rejInfos []RejectInfo
	for _, rej := range rejections {
		rejInfos = append(rejInfos, RejectInfo{TsCode: rej.TsCode, Reason: rej.Reason, Rule: rej.Rule})
		logger.L().Warnf("[%s] 风控拦截 %s: %s (%s)", in.Date, rej.TsCode, rej.Reason, rej.Rule)
	}

	sortSignalsByPriority(funded)
	return funded, rejInfos
}

// sortSignalsByPriority 卖出在前, 同方向按信号强度降序
// 卖出优先: 先卖才能释放资金; 强度优先: 批次额度 (持仓数 / 现金) 先到先得
func sortSignalsByPriority(signals []model.Signal) {
	sort.SliceStable(signals, func(i, j int) bool {
		si, sj := signals[i].Direction == model.DirSell, signals[j].Direction == model.DirSell
		if si != sj {
			return si
		}
		return signals[i].Strength > signals[j].Strength
	})
}

// applyFunding 按可用资金核算本批次买入计划
//
// 仓位上限的口径是"占总资产比例", 只有 max_total_position_pct<1 时才间接留出安全垫;
// 同日多笔买入逐笔看都合规, 叠加后却可能超出账户现金 —— 计划落到人工执行就是"下不了单"。
// 因此以批次为单位做真金白银核算: 先计入本批次卖出的净回款 (计划按"先卖后买"执行),
// 再按信号强度从高到低扣减; 买不满的缩量, 连一手都买不起的明确拒因。
func applyFunding(in SignalInput, signals []model.Signal) ([]model.Signal, []risk.RejectReason) {
	if in.Cash <= 0 {
		return signals, nil // 无资金快照时不假装约束: 交给仓位比例上限兜底
	}
	var sells, buys []model.Signal
	available := in.Cash
	sortSignalsByPriority(signals) // 资金不够买全部时保留强者 (与风控占额同一口径)
	for _, sig := range signals {
		if sig.Direction == model.DirSell {
			sells = append(sells, sig)
			available += in.sellIncome(sig)
			continue
		}
		buys = append(buys, sig)
	}

	out := make([]model.Signal, 0, len(signals))
	out = append(out, sells...)
	var rejected []risk.RejectReason
	for _, sig := range buys {
		price := in.priceOf(sig)
		if price <= 0 {
			out = append(out, sig) // 无价格无法核算, 原样放行并由日志留痕
			continue
		}
		qty, cost := affordableLot(price, sig.TargetQty, available, in.buyCost)
		if qty <= 0 {
			rejected = append(rejected, rejectFunding(sig, price, available))
			continue
		}
		if qty < sig.TargetQty {
			sig.TargetQty = qty
			sig.Reason = fmt.Sprintf("%s (资金调整: 可买%d股)", sig.Reason, qty)
		}
		available -= cost
		out = append(out, sig)
	}
	return out, rejected
}

// affordableLot 从请求量逐手递减到可用资金能覆盖的最大整手数 (含费用)
// 一手都买不起时返回 0
func affordableLot(price float64, wantQty int, available float64, costOf func(float64, int) float64) (int, float64) {
	qty := market.RoundLot(wantQty)
	for qty >= market.LotSize {
		cost := costOf(price, qty)
		if cost <= available {
			return qty, cost
		}
		qty -= market.LotSize
	}
	return 0, 0
}

// rejectFunding 构造"资金不足"拒绝原因
func rejectFunding(sig model.Signal, price, available float64) risk.RejectReason {
	return risk.RejectReason{
		TsCode: sig.TsCode,
		Signal: sig,
		Rule:   "insufficient_cash",
		Reason: fmt.Sprintf("可用资金不足: 一手需 %.0f, 本批次剩余可用 %.0f", price*market.LotSize, available),
	}
}

// priceOf 信号标的的核算价 (当日收盘), 缺失返回 0
func (in SignalInput) priceOf(sig model.Signal) float64 {
	if bar := in.Bars[sig.TsCode]; bar != nil {
		return bar.Close
	}
	if pos := in.Positions[sig.TsCode]; pos != nil {
		return pos.MarketPrice
	}
	return 0
}

// buyCost 买入总花费 (含费用), 未注入费用模型时退回成交额毛额
func (in SignalInput) buyCost(price float64, qty int) float64 {
	if in.Cost == nil {
		return price * float64(qty)
	}
	return in.Cost.BuyCost(price, qty)
}

// sellIncome 卖出净回款 (扣费用), 未注入费用模型时退回成交额毛额
func (in SignalInput) sellIncome(sig model.Signal) float64 {
	price := in.priceOf(sig)
	if price <= 0 || sig.TargetQty <= 0 {
		return 0
	}
	if in.Cost == nil {
		return price * float64(sig.TargetQty)
	}
	return in.Cost.SellIncome(price, sig.TargetQty)
}

// StopCodesOf 从止损信号构建标的集合
func StopCodesOf(stopSignals []model.Signal) map[string]bool {
	codes := make(map[string]bool, len(stopSignals))
	for _, s := range stopSignals {
		codes[s.TsCode] = true
	}
	return codes
}
