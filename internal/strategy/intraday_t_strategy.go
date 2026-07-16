package strategy

import (
	"context"
	"fmt"
	"math"

	"jingzhe-trader/internal/indicator"
	"jingzhe-trader/internal/model"
)

// IntradayTStrategy 日内做T策略（持仓增强策略）
//
// 策略定位：
//   - 这不是一个独立的选股策略，而是在已有底仓基础上的持仓增强策略
//   - 利用A股T+1制度下的底仓，通过日内高抛低吸降低持仓成本
//   - 需要与主策略（如均线交叉、多因子等）配合使用，不能替代主策略
//
// 做T方式：
//   - 先卖后买（倒T）：高开冲高时卖出部分底仓，回落时买回，赚取差价
//   - 先买后卖（正T）：低开下探时买入等量股份，反弹时卖出底仓，赚取差价
//
// 适用场景：
//   - 已有底仓的股票（底仓数量越多，做T空间越大）
//   - 日内波动较大的股票（振幅通常大于1%）
//   - 震荡市效果最佳，单边趋势市容易T飞或套牢
//
// 风险提示：
//   - 做T有成本（佣金、印花税、过户费），目标收益需覆盖交易成本
//   - 单边上涨行情中做倒T可能卖飞筹码
//   - 单边下跌行情中做正T可能越补越跌
type IntradayTStrategy struct {
	// 配置参数
	TAmountPct   float64 // 每次做T使用的仓位占底仓比例，默认0.3（30%）
	ProfitTarget float64 // 做T目标利润率，默认0.015（1.5%）
	StopLossPct  float64 // 做T止损比例，默认-0.01（-1%）
	LookbackDays int     // 计算波动率参考的历史天数，默认20
	ATRPeriod    int     // ATR周期，默认14

	// 内部状态
	tPositions map[string]*tPosition // 每只股票的做T状态
}

// tPosition 单只股票的做T状态
// 用于跟踪日内做T的开仓和平仓情况
type tPosition struct {
	TsCode        string  // 股票代码
	BaseQty       int     // 底仓数量（可用于做T的最大数量）
	AvailableTQty int     // 今日可用做T数量
	Mode          string  // 做T模式："none"/"buy_first"(正T)/"sell_first"(倒T)
	EntryPrice    float64 // 做T入场价
	EntryQty      int     // 做T入场数量
	EntryTime     string  // 做T入场时间
	Status        string  // 状态："idle"(空闲)/"in_trade"(持仓中)
}

// NewIntradayTStrategy 创建日内做T策略
func NewIntradayTStrategy() *IntradayTStrategy {
	return &IntradayTStrategy{
		TAmountPct:   0.3,
		ProfitTarget: 0.015,
		StopLossPct:  -0.01,
		LookbackDays: 20,
		ATRPeriod:    14,
		tPositions:   make(map[string]*tPosition),
	}
}

// Name 返回策略名称
func (s *IntradayTStrategy) Name() string { return "intraday_t" }

// Init 初始化策略参数
// 支持的参数：
//   - t_amount_pct: 每次做T使用的仓位占比 (0, 1]
//   - profit_target: 做T目标利润率 (>0)
//   - stop_loss_pct: 做T止损比例 (<0)
//   - lookback_days: 历史回看天数 (>0)
//   - atr_period: ATR计算周期 (>0)
func (s *IntradayTStrategy) Init(_ context.Context, params map[string]interface{}) error {
	if v, ok := params["t_amount_pct"]; ok {
		if p, ok := v.(float64); ok && p > 0 && p <= 1 {
			s.TAmountPct = p
		}
	}
	if v, ok := params["profit_target"]; ok {
		if p, ok := v.(float64); ok && p > 0 {
			s.ProfitTarget = p
		}
	}
	if v, ok := params["stop_loss_pct"]; ok {
		if p, ok := v.(float64); ok && p < 0 {
			s.StopLossPct = p
		}
	}
	if v, ok := params["lookback_days"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			s.LookbackDays = int(n)
		}
	}
	if v, ok := params["atr_period"]; ok {
		if n, ok := v.(float64); ok && n > 0 {
			s.ATRPeriod = int(n)
		}
	}
	return nil
}

// OnBar 日线级别入口（用于评估当日是否适合做T）
//
// 注意：
//   - 真正的做T交易需要分钟级行情数据来捕捉日内买卖点
//   - 这里提供的是日线级别的"今日适合做T"评估建议
//   - 生成的信号为做T建议信号，Direction 使用 DirBuy 表示"建议做T"
//   - 具体的入场出场点计算需配合分钟级数据调用 CalcTEntryPoint / CalcTExitPoint
//
// 信号说明：
//   - Direction: DirBuy 表示今日建议做T（仅为建议标记，非真实买入信号）
//   - TargetQty: 建议做T的数量（已按100股取整）
//   - Reason: 包含平均振幅、目标收益、做T数量等信息
//   - Strength: 做T适宜程度，波动越大强度越高
func (s *IntradayTStrategy) OnBar(_ context.Context, barCtx *BarContext) ([]model.Signal, error) {
	var signals []model.Signal

	for tsCode, pos := range barCtx.Positions {
		if pos.TotalQty <= 0 {
			continue
		}

		// 获取历史K线数据用于计算ATR和振幅
		bars, err := barCtx.History.GetBars(tsCode, barCtx.TradeDate, s.LookbackDays)
		if err != nil || len(bars) < s.ATRPeriod+1 {
			continue
		}

		bar, ok := barCtx.Bars[tsCode]
		if !ok || bar.Close <= 0 {
			continue
		}

		// 提取高低收价格序列
		highs := make([]float64, len(bars))
		lows := make([]float64, len(bars))
		closes := make([]float64, len(bars))
		for i, b := range bars {
			highs[i] = b.High
			lows[i] = b.Low
			closes[i] = b.Close
		}

		// 计算ATR（平均真实波幅）
		atrValues := indicator.ATR(highs, lows, closes, s.ATRPeriod)
		latestATR := atrValues[len(atrValues)-1]
		if math.IsNaN(latestATR) || latestATR <= 0 {
			continue
		}

		// ATR占比 = ATR / 收盘价，衡量日内波动幅度
		atrPct := latestATR / bar.Close
		if atrPct < 0.01 {
			continue // 日内波动太小，不适合做T
		}

		// 计算做T数量（底仓的 TAmountPct，向下取整到100股）
		tQty := int(float64(pos.TotalQty)*s.TAmountPct/100) * 100
		if tQty < 100 {
			continue // 数量太少不值得做T
		}

		// 估算做T预期收益
		expectedProfit := bar.Close * float64(tQty) * s.ProfitTarget

		// 估算交易成本（佣金+印花税+过户费，粗略按万分之八估算）
		tradeCost := bar.Close * float64(tQty) * 0.0008 * 2 // 买卖双边
		if expectedProfit <= tradeCost {
			continue // 预期收益不足以覆盖成本
		}

		// 信号强度：波动越大、底仓越多，强度越高
		strength := math.Min(atrPct/0.03, 1.0) * 0.7 // 波动贡献最多0.7
		if pos.TotalQty >= 10000 {
			strength += 0.1 // 底仓充足加0.1
		}
		if pos.CostPrice > 0 && bar.Close > pos.CostPrice {
			strength += 0.1 // 盈利状态做T更安全
		}
		strength = math.Min(strength, 0.9)

		// 更新做T状态
		s.tPositions[tsCode] = &tPosition{
			TsCode:        tsCode,
			BaseQty:       pos.TotalQty,
			AvailableTQty: tQty,
			Mode:          "none",
			Status:        "idle",
		}

		signal := model.Signal{
			TsCode:    tsCode,
			Direction: model.DirBuy, // 用 DirBuy 标记"建议做T"
			TargetQty: tQty,
			Reason: fmt.Sprintf(
				"日内做T建议: ATR振幅%.2f%%, 目标收益%.1f%%, 数量%d股, 预期盈利%.0f元",
				atrPct*100, s.ProfitTarget*100, tQty, expectedProfit,
			),
			Strength: strength,
		}
		signals = append(signals, signal)
	}

	return signals, nil
}

// ShouldDoT 判断某只股票今天是否适合做T
//
// 参数：
//   - tsCode: 股票代码
//   - currentPrice: 当前价格
//   - pos: 持仓信息
//   - closes: 历史收盘价序列
//
// 返回：
//   - bool: 是否适合做T
//   - int: 建议做T数量（100股取整）
//   - float64: 预期收益金额
func (s *IntradayTStrategy) ShouldDoT(tsCode string, currentPrice float64, pos *model.Position, closes []float64) (bool, int, float64) {
	if pos == nil || pos.TotalQty < 200 {
		return false, 0, 0 // 底仓太少
	}
	if currentPrice <= 0 {
		return false, 0, 0
	}

	avgRange := s.avgDailyRange(closes)
	if avgRange < 0.01 {
		return false, 0, 0 // 波动太小
	}

	tQty := int(float64(pos.TotalQty)*s.TAmountPct/100) * 100
	if tQty < 100 {
		return false, 0, 0
	}

	expectedProfit := currentPrice * float64(tQty) * s.ProfitTarget
	return true, tQty, expectedProfit
}

// CalcTEntryPoint 计算做T入场点
//
// 参数：
//   - closes: 历史收盘价序列
//   - currentPrice: 当前价格
//   - direction: "buy" 先买后卖的买点（正T买点），"sell" 先卖后买的卖点（倒T卖点）
//
// 返回：建议的入场价格
//
// 计算逻辑：
//   - 正T买点：现价 - 0.5 * 平均振幅（下探时买入）
//   - 倒T卖点：现价 + 0.5 * 平均振幅（冲高时卖出）
func (s *IntradayTStrategy) CalcTEntryPoint(closes []float64, currentPrice float64, direction string) float64 {
	avgRange := s.avgDailyRange(closes)
	if direction == "buy" {
		// 下探时买入：现价 - 0.5 * 平均振幅
		return currentPrice * (1 - avgRange*0.5)
	}
	// 冲高时卖出：现价 + 0.5 * 平均振幅
	return currentPrice * (1 + avgRange*0.5)
}

// CalcTExitPoint 计算做T出场点（止盈价）
//
// 参数：
//   - entryPrice: 入场价格
//   - direction: "buy" 正T（先买后卖，出场是卖出），"sell" 倒T（先卖后买，出场是买回）
//
// 返回：止盈出场价格
func (s *IntradayTStrategy) CalcTExitPoint(entryPrice float64, direction string) float64 {
	if direction == "buy" {
		// 正T：买入后反弹卖出止盈
		return entryPrice * (1 + s.ProfitTarget)
	}
	// 倒T：卖出后回落买回止盈
	return entryPrice * (1 - s.ProfitTarget)
}

// CalcTStopLossPoint 计算做T止损价
//
// 参数：
//   - entryPrice: 入场价格
//   - direction: "buy" 正T，"sell" 倒T
//
// 返回：止损价格
func (s *IntradayTStrategy) CalcTStopLossPoint(entryPrice float64, direction string) float64 {
	if direction == "buy" {
		// 正T止损：买入后继续下跌，跌破止损价卖出底仓止损
		return entryPrice * (1 + s.StopLossPct)
	}
	// 倒T止损：卖出后继续上涨，突破止损价买回止损
	return entryPrice * (1 - s.StopLossPct)
}

// avgDailyRange 计算平均日振幅
// 用历史日涨跌幅的绝对值近似日内振幅
//
// 注意：精确的日内振幅应该用 (high-low)/close，
// 这里提供的是基于收盘价的简化估算版本。
// 如需更精确的计算，应使用 ATR 指标（需要 high/low 数据）。
func (s *IntradayTStrategy) avgDailyRange(closes []float64) float64 {
	if len(closes) < 2 {
		return 0
	}
	var sum float64
	count := 0
	for i := 1; i < len(closes); i++ {
		if closes[i-1] > 0 {
			dailyChange := math.Abs(closes[i]/closes[i-1] - 1)
			sum += dailyChange
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

// GetTPosition 获取某只股票的做T状态
func (s *IntradayTStrategy) GetTPosition(tsCode string) (*tPosition, bool) {
	pos, ok := s.tPositions[tsCode]
	return pos, ok
}

// ResetDaily 每日开盘前重置做T状态
// 每个交易日开盘前调用，重置日内做T状态
func (s *IntradayTStrategy) ResetDaily() {
	for tsCode := range s.tPositions {
		s.tPositions[tsCode].Mode = "none"
		s.tPositions[tsCode].Status = "idle"
		s.tPositions[tsCode].EntryPrice = 0
		s.tPositions[tsCode].EntryQty = 0
		s.tPositions[tsCode].EntryTime = ""
	}
}
