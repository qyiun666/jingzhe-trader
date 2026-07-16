package strategy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/factor"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
)

// MultiFactorStrategy 多因子选股策略
// 在调仓日计算全股票池综合因子得分，选取前N只构建持仓
// 因子组合：价值(PE/PB) + 质量(ROE/毛利率) + 成长(净利润同比) + 动量(60日涨幅) + 情绪(换手率/量比/涨跌停)
type MultiFactorStrategy struct {
	// 配置参数
	TopN           int     // 选前N只股票, 默认10
	RebalanceFreq  string  // 调仓频率: weekly / monthly, 默认monthly
	PositionPct    float64 // 单票仓位占比, 默认按 topN 均仓 (1/topN)
	StopLossPct    float64 // 止损比例, 默认-0.15 (-15%)
	TakeProfitPct  float64 // 止盈比例, 默认0.30 (30%)
	MomentumPeriod int     // 动量因子周期, 默认60日

	// 自适应参数
	EnableAdaptive bool            // 是否启用自适应参数调整
	adaptive       *AdaptiveParams // 自适应参数调整器实例

	// 依赖注入
	factorProvider factor.DataProvider // 因子数据提供者
	calendar       *market.Calendar    // 交易日历 (用于判断调仓日)

	// 内部状态
	compositeFactor *factor.CompositeFactor // 复合因子实例
	targetPortfolio map[string]bool         // 目标持仓集合 (调仓日更新)
	lastRebalance   string                  // 上次调仓日期
}

// NewMultiFactorStrategy 创建多因子选股策略
func NewMultiFactorStrategy() *MultiFactorStrategy {
	return &MultiFactorStrategy{
		TopN:           10,
		RebalanceFreq:  "monthly",
		PositionPct:    0, // 0 表示按 topN 均仓
		StopLossPct:    -0.15,
		TakeProfitPct:  0.30,
		MomentumPeriod: 60,
		EnableAdaptive: false,
	}
}

// Name 返回策略名称
func (s *MultiFactorStrategy) Name() string {
	return "multi_factor"
}

// SetFactorDataProvider 设置因子数据提供者 (依赖注入)
func (s *MultiFactorStrategy) SetFactorDataProvider(p factor.DataProvider) {
	s.factorProvider = p
}

// SetCalendar 设置交易日历
func (s *MultiFactorStrategy) SetCalendar(cal *market.Calendar) {
	s.calendar = cal
}

// Init 初始化策略参数
func (s *MultiFactorStrategy) Init(_ context.Context, params map[string]interface{}) error {
	// 解析参数
	if v, ok := params["top_n"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			s.TopN = int(n)
		}
	}
	if v, ok := params["rebalance_freq"]; ok {
		if freq, ok := v.(string); ok {
			freq = strings.ToLower(freq)
			if freq == "weekly" || freq == "monthly" {
				s.RebalanceFreq = freq
			}
		}
	}
	if v, ok := params["position_pct"]; ok {
		if pct, ok := v.(float64); ok {
			s.PositionPct = pct
		}
	}
	if v, ok := params["stop_loss_pct"]; ok {
		if pct, ok := v.(float64); ok {
			s.StopLossPct = pct
		}
	}
	if v, ok := params["take_profit_pct"]; ok {
		if pct, ok := v.(float64); ok {
			s.TakeProfitPct = pct
		}
	}
	if v, ok := params["momentum_period"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			s.MomentumPeriod = int(n)
		}
	}
	if v, ok := params["enable_adaptive"]; ok {
		if b, ok := v.(bool); ok {
			s.EnableAdaptive = b
		}
	}

	// 如果未指定单票仓位, 按 topN 均仓
	if s.PositionPct <= 0 {
		s.PositionPct = 1.0 / float64(s.TopN)
	}

	// 如果启用自适应, 创建自适应参数调整器
	if s.EnableAdaptive {
		s.adaptive = NewAdaptiveParams()
		s.adaptive.SetBaseParams(5, 20, s.StopLossPct, s.TakeProfitPct, s.PositionPct)
	}

	// 构建复合因子
	s.buildCompositeFactor()

	return nil
}

// buildCompositeFactor 构建复合因子
// 因子组合：价值(PE+PB) + 质量(ROE+毛利率) + 成长(净利润同比) + 动量(60日涨幅) + 情绪(换手率+量比+涨跌停)
// 权重分配：价值0.20 + 质量0.25 + 成长0.15 + 动量0.20 + 情绪0.20
func (s *MultiFactorStrategy) buildCompositeFactor() {
	factors := []factor.FactorWeight{
		// 价值因子 (权重 0.20)
		{Factor: &factor.PEFactor{}, Weight: 0.10},
		{Factor: &factor.PBFactor{}, Weight: 0.10},
		// 质量因子 (权重 0.25)
		{Factor: &factor.ROEFactor{}, Weight: 0.125},
		{Factor: &factor.GrossMarginFactor{}, Weight: 0.125},
		// 成长因子 (权重 0.15)
		{Factor: &factor.NetProfitYoyFactor{}, Weight: 0.15},
		// 动量因子 (权重 0.20)
		{Factor: &factor.MomentumFactor{Period: s.MomentumPeriod}, Weight: 0.20},
		// 情绪因子 (权重 0.20)
		{Factor: &factor.TurnoverFactor{Period: 20}, Weight: 0.08},
		{Factor: &factor.VolumeRatioFactor{ShortPeriod: 5, LongPeriod: 20}, Weight: 0.06},
		{Factor: &factor.LimitMotionFactor{Period: 20}, Weight: 0.06},
	}

	s.compositeFactor = factor.NewCompositeFactor("multi_factor", factors)
}

// OnBar 每个交易日触发，返回交易信号
// 调仓日：计算综合得分 -> 确定目标持仓 -> 生成买卖信号
// 非调仓日：检查止损止盈
func (s *MultiFactorStrategy) OnBar(ctx context.Context, barCtx *BarContext) ([]model.Signal, error) {
	if s.factorProvider == nil {
		return nil, fmt.Errorf("因子数据提供者未设置, 请调用 SetFactorDataProvider")
	}
	if s.compositeFactor == nil {
		return nil, fmt.Errorf("复合因子未初始化, 请先调用 Init")
	}

	// 如果启用自适应, 先更新波动率状态 (使用第一只持仓股票或股票池第一只的ATR)
	if s.EnableAdaptive && s.adaptive != nil {
		s.updateVolatility(barCtx)
	}

	var signals []model.Signal

	// 判断是否为调仓日
	isRebalanceDay := s.isRebalanceDay(barCtx.TradeDate)

	if isRebalanceDay {
		// 调仓日：重新选股并调仓
		rebalanceSignals, err := s.doRebalance(ctx, barCtx)
		if err != nil {
			return nil, fmt.Errorf("调仓失败: %w", err)
		}
		signals = append(signals, rebalanceSignals...)
		s.lastRebalance = barCtx.TradeDate
	} else {
		// 非调仓日：检查止损止盈
		stopSignals := s.checkStopLossProfit(barCtx)
		signals = append(signals, stopSignals...)
	}

	return signals, nil
}

// updateVolatility 更新市场波动率状态
// 选取第一只持仓股票或股票池第一只股票来计算市场波动率
func (s *MultiFactorStrategy) updateVolatility(barCtx *BarContext) {
	if s.adaptive == nil {
		return
	}

	// 优先使用第一只持仓股票
	var tsCode string
	for code := range barCtx.Positions {
		if barCtx.Positions[code].TotalQty > 0 {
			tsCode = code
			break
		}
	}
	// 如果没有持仓, 使用股票池第一只
	if tsCode == "" && len(barCtx.Universe) > 0 {
		tsCode = barCtx.Universe[0]
	}
	if tsCode == "" {
		return
	}

	// 获取历史K线用于计算ATR (需要至少 ATR周期 + 1 根)
	atrLen := 250 // 取足够长的历史
	bars, err := barCtx.History.GetBars(tsCode, barCtx.TradeDate, atrLen)
	if err != nil || len(bars) < 15 {
		return
	}

	// 提取价格序列
	closes := make([]float64, len(bars))
	highs := make([]float64, len(bars))
	lows := make([]float64, len(bars))
	for i, bar := range bars {
		closes[i] = bar.AdjClose()
		highs[i] = bar.AdjHigh()
		lows[i] = bar.AdjLow()
	}

	s.adaptive.Update(closes, highs, lows)
}

// isRebalanceDay 判断是否为调仓日
func (s *MultiFactorStrategy) isRebalanceDay(date string) bool {
	if s.calendar == nil {
		// 没有日历时, 假设每天都是调仓日 (退化模式)
		return true
	}
	if !s.calendar.IsTradeDate(date) {
		return false
	}

	switch s.RebalanceFreq {
	case "monthly":
		// 月末调仓
		return s.calendar.IsLastTradeDateOfMonth(date)
	case "weekly":
		// 周度调仓 (每周最后一个交易日, 即周五)
		return s.isLastTradeDateOfWeek(date)
	default:
		return s.calendar.IsLastTradeDateOfMonth(date)
	}
}

// isLastTradeDateOfWeek 判断是否为当周最后一个交易日
// 判断逻辑: 若 date 的下一个交易日已属于下一个自然周, 则 date 为当周最后一个交易日
func (s *MultiFactorStrategy) isLastTradeDateOfWeek(date string) bool {
	next := s.calendar.NextTradeDate(date)
	if next == "" {
		// 没有下一个交易日, 视为周末 (同时也是日历中最后一个交易日)
		return true
	}

	t, err := time.Parse("20060102", date)
	if err != nil {
		return false
	}
	nextT, err := time.Parse("20060102", next)
	if err != nil {
		return false
	}

	// 计算两个日期分别是一年中的第几周
	_, week1 := t.ISOWeek()
	_, week2 := nextT.ISOWeek()

	return week1 != week2
}

// doRebalance 执行调仓
func (s *MultiFactorStrategy) doRebalance(ctx context.Context, barCtx *BarContext) ([]model.Signal, error) {
	// 1. 计算全股票池综合因子得分
	results, err := s.compositeFactor.Compute(ctx, barCtx.TradeDate, barCtx.Universe, s.factorProvider)
	if err != nil {
		return nil, fmt.Errorf("计算复合因子失败: %w", err)
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("无有效因子数据")
	}

	// 2. 取前 N 只作为目标持仓
	n := s.TopN
	if n > len(results) {
		n = len(results)
	}
	targetSet := make(map[string]bool, n)
	targetScores := make(map[string]float64, n)
	for i := 0; i < n; i++ {
		targetSet[results[i].TsCode] = true
		targetScores[results[i].TsCode] = results[i].Score
	}
	s.targetPortfolio = targetSet

	// 获取当前使用的仓位比例 (自适应模式下动态调整)
	positionPct := s.PositionPct
	if s.EnableAdaptive && s.adaptive != nil {
		positionPct = s.adaptive.GetPositionSize()
	}

	var signals []model.Signal

	// 3. 卖出不在目标持仓中的股票
	for tsCode, pos := range barCtx.Positions {
		if pos.TotalQty > 0 && !targetSet[tsCode] {
			bar, ok := barCtx.Bars[tsCode]
			if !ok || bar.Close <= 0 {
				continue
			}
			reason := "多因子调仓: 调出股票池"
			if s.EnableAdaptive {
				status := s.adaptive.GetStatus()
				reason = fmt.Sprintf("%s [%s]", reason, status.VolatilityDesc)
			}
			signals = append(signals, model.Signal{
				TsCode:    tsCode,
				Direction: model.DirSell,
				TargetQty: pos.TotalQty,
				Reason:    reason,
				Strength:  0.7,
			})
		}
	}

	// 4. 计算可分配资金 (用于买入新股票)
	totalAsset := barCtx.TotalAsset
	targetAmountPerStock := totalAsset * positionPct

	// 5. 买入目标持仓中尚未持有的股票
	for i := 0; i < n; i++ {
		tsCode := results[i].TsCode
		score := results[i].Score

		// 已持有则跳过 (不做加仓, 保持均仓)
		if pos, ok := barCtx.Positions[tsCode]; ok && pos.TotalQty > 0 {
			continue
		}

		bar, ok := barCtx.Bars[tsCode]
		if !ok || bar.Close <= 0 {
			continue
		}

		// 计算买入数量 (向下取整到100股)
		price := bar.AdjClose()
		if price <= 0 {
			price = bar.Close
		}
		qty := int(targetAmountPerStock/price/100) * 100
		if qty <= 0 {
			continue
		}

		// 检查现金是否足够 (粗略估计)
		if float64(qty)*price > barCtx.Cash {
			// 现金不足则减少买入量
			qty = int(barCtx.Cash/price/100) * 100
			if qty <= 0 {
				continue
			}
		}

		reason := fmt.Sprintf("多因子选股: 综合得分%.2f, 排名第%d", score, i+1)
		if s.EnableAdaptive {
			status := s.adaptive.GetStatus()
			reason = fmt.Sprintf("%s [%s, 仓位%.1f%%]", reason, status.VolatilityDesc, positionPct*100)
		}
		signals = append(signals, model.Signal{
			TsCode:    tsCode,
			Direction: model.DirBuy,
			TargetQty: qty,
			Reason:    reason,
			Strength:  0.8,
		})
	}

	return signals, nil
}

// checkStopLossProfit 检查止损止盈
func (s *MultiFactorStrategy) checkStopLossProfit(barCtx *BarContext) []model.Signal {
	var signals []model.Signal

	// 获取当前使用的止损止盈比例 (自适应模式下动态调整)
	stopLossPct := s.StopLossPct
	takeProfitPct := s.TakeProfitPct
	if s.EnableAdaptive && s.adaptive != nil {
		stopLossPct = s.adaptive.GetStopLoss()
		takeProfitPct = s.adaptive.GetTakeProfit()
	}

	for tsCode, pos := range barCtx.Positions {
		if pos.TotalQty <= 0 {
			continue
		}
		if pos.CostPrice <= 0 {
			continue
		}

		bar, ok := barCtx.Bars[tsCode]
		if !ok || bar.Close <= 0 {
			continue
		}

		// 计算浮盈比例
		profitPct := (bar.Close - pos.CostPrice) / pos.CostPrice

		// 止损
		if profitPct <= stopLossPct {
			reason := fmt.Sprintf("止损触发: 浮亏%.2f%%, 成本%.2f, 现价%.2f", profitPct*100, pos.CostPrice, bar.Close)
			if s.EnableAdaptive {
				status := s.adaptive.GetStatus()
				reason = fmt.Sprintf("%s [%s, 止损线%.1f%%]", reason, status.VolatilityDesc, stopLossPct*100)
			}
			signals = append(signals, model.Signal{
				TsCode:    tsCode,
				Direction: model.DirSell,
				TargetQty: pos.TotalQty,
				Reason:    reason,
				Strength:  0.9,
			})
			continue
		}

		// 止盈
		if profitPct >= takeProfitPct {
			reason := fmt.Sprintf("止盈触发: 浮盈%.2f%%, 成本%.2f, 现价%.2f", profitPct*100, pos.CostPrice, bar.Close)
			if s.EnableAdaptive {
				status := s.adaptive.GetStatus()
				reason = fmt.Sprintf("%s [%s, 止盈线%.1f%%]", reason, status.VolatilityDesc, takeProfitPct*100)
			}
			signals = append(signals, model.Signal{
				TsCode:    tsCode,
				Direction: model.DirSell,
				TargetQty: pos.TotalQty,
				Reason:    reason,
				Strength:  0.9,
			})
		}
	}

	return signals
}
