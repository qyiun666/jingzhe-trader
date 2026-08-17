package engine

// 统一信号汇集: 回测 Pipeline 与实盘计划生成共用同一套
// 合并/过滤/风控检查/排序语义, 防止两套实现漂移
//
// 约定:
//   - 止损/止盈信号优先, 策略对同一股票的信号剔除
//   - Advice 建议类信号 (如做T提示) 不进成交管道, 仅记日志
//   - 全部信号统一过风控检查, 拒绝原因返回给调用方展示
//   - 结果按 "卖出在前" 排序 (先卖出释放资金再买入)

import (
	"sort"

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

// CheckAndSortSignals 统一过风控检查并排序 (卖出在前)
// 返回: 通过信号; 拒绝列表用于展示/告警
func CheckAndSortSignals(date string, rm *risk.RiskManager, signals []model.Signal,
	positions map[string]*model.Position, totalAsset float64,
	stocks map[string]*model.Stock, bars map[string]*model.Bar) ([]model.Signal, []RejectInfo) {

	passed, rejections := rm.Check(signals, positions, totalAsset, stocks, date, bars)
	var rejInfos []RejectInfo
	for _, rej := range rejections {
		rejInfos = append(rejInfos, RejectInfo{TsCode: rej.TsCode, Reason: rej.Reason, Rule: rej.Rule})
		logger.L().Warnf("[%s] 风控拦截 %s: %s (%s)", date, rej.TsCode, rej.Reason, rej.Rule)
	}

	// 卖出排在买入前, 先释放资金
	sort.SliceStable(passed, func(i, j int) bool {
		return passed[i].Direction == model.DirSell && passed[j].Direction != model.DirSell
	})
	return passed, rejInfos
}

// StopCodesOf 从止损信号构建标的集合
func StopCodesOf(stopSignals []model.Signal) map[string]bool {
	codes := make(map[string]bool, len(stopSignals))
	for _, s := range stopSignals {
		codes[s.TsCode] = true
	}
	return codes
}
