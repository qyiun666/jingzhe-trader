package strategy

import (
	"context"
	"fmt"
	"math"

	"jingzhe-trader/internal/indicator"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// MACrossStrategy 均线交叉策略
// 金叉(短均线上穿长均线)买入, 死叉(短均线下穿长均线)卖出
type MACrossStrategy struct {
	ShortPeriod int     // 短均线周期 (默认5)
	LongPeriod  int     // 长均线周期 (默认20)
	HistoryLen  int     // 需要的历史数据长度
	PositionPct float64 // 单票仓位占比 (默认0.1 = 10%)

	// 自适应参数
	EnableAdaptive bool            // 是否启用自适应参数调整
	adaptive       *AdaptiveParams // 自适应参数调整器实例

	// ===== 信号过滤配置 (减少假信号, 提高胜率) =====
	lastSignalDate   map[string]string // 每只股票上次同方向信号日期, key=tsCode+"|buy"/"|sell", value=YYYYMMDD
	buyCooldownDays  int               // 金叉(买入)信号冷却期交易日, 默认5
	sellCooldownDays int               // 死叉(卖出)信号冷却期交易日, 默认3
	volConfirm       bool              // 是否启用成交量确认
	trendConfirm     bool              // 是否启用趋势强度确认
	marketFilter     bool              // 是否启用大盘环境过滤

	// 过滤阈值参数
	volConfirmRatio     float64 // 成交量确认倍数, 默认1.2
	trendThreshold      float64 // 短长均线差距阈值, 默认0.005 (0.5%)
	marketDropThreshold float64 // 大盘跌幅阈值(小数), 默认-0.02 (-2%)
	marketIndexCode     string  // 大盘指数代码, 默认000300.SH
}

func (s *MACrossStrategy) Name() string { return "ma_cross" }

func (s *MACrossStrategy) Init(_ context.Context, params map[string]interface{}) error {
	s.ShortPeriod = 5
	s.LongPeriod = 20
	s.PositionPct = 0.1
	s.HistoryLen = 60
	s.EnableAdaptive = false

	if v, ok := params["short_period"]; ok {
		if n, ok := v.(float64); ok {
			s.ShortPeriod = int(n)
		}
	}
	if v, ok := params["long_period"]; ok {
		if n, ok := v.(float64); ok {
			s.LongPeriod = int(n)
		}
	}
	if v, ok := params["position_pct"]; ok {
		if n, ok := v.(float64); ok {
			s.PositionPct = n
		}
	}
	if v, ok := params["enable_adaptive"]; ok {
		if b, ok := v.(bool); ok {
			s.EnableAdaptive = b
		}
	}

	// ===== 信号过滤参数 (默认全部开启, 可通过 params 关闭) =====
	s.volConfirm = true
	s.trendConfirm = true
	s.marketFilter = true
	s.buyCooldownDays = 5
	s.sellCooldownDays = 3
	s.volConfirmRatio = 1.2
	s.trendThreshold = 0.005
	s.marketDropThreshold = -0.02
	s.marketIndexCode = "000300.SH"
	s.lastSignalDate = make(map[string]string)

	if v, ok := params["vol_confirm"]; ok {
		if b, ok := v.(bool); ok {
			s.volConfirm = b
		}
	}
	if v, ok := params["trend_confirm"]; ok {
		if b, ok := v.(bool); ok {
			s.trendConfirm = b
		}
	}
	if v, ok := params["market_filter"]; ok {
		if b, ok := v.(bool); ok {
			s.marketFilter = b
		}
	}
	if v, ok := params["buy_cooldown_days"]; ok {
		if n, ok := v.(float64); ok {
			s.buyCooldownDays = int(n)
		}
	}
	if v, ok := params["sell_cooldown_days"]; ok {
		if n, ok := v.(float64); ok {
			s.sellCooldownDays = int(n)
		}
	}
	if v, ok := params["vol_confirm_ratio"]; ok {
		if n, ok := v.(float64); ok {
			s.volConfirmRatio = n
		}
	}
	if v, ok := params["trend_threshold"]; ok {
		if n, ok := v.(float64); ok {
			s.trendThreshold = n
		}
	}
	if v, ok := params["market_drop_threshold"]; ok {
		if n, ok := v.(float64); ok {
			s.marketDropThreshold = n
		}
	}
	if v, ok := params["market_index_code"]; ok {
		if str, ok := v.(string); ok {
			s.marketIndexCode = str
		}
	}

	// 如果启用自适应, 创建自适应参数调整器
	if s.EnableAdaptive {
		s.adaptive = NewAdaptiveParams()
		s.adaptive.SetBaseParams(s.ShortPeriod, s.LongPeriod, -0.15, 0.30, s.PositionPct)
		// 自适应模式下, 历史数据长度需要额外增加 ATR 周期 + 250天波动率历史
		// 同时均线周期可能被拉长, 取最大可能值的3倍
		s.HistoryLen = 250 + 60 // 250天波动率历史 + 60天指标计算缓冲
	} else {
		s.HistoryLen = s.LongPeriod * 3
	}

	return nil
}

func (s *MACrossStrategy) OnBar(_ context.Context, barCtx *BarContext) ([]model.Signal, error) {
	var signals []model.Signal

	// 预计算当日大盘是否处于系统性下跌 (买入过滤), 全场只查询一次
	marketBlocked := false
	if s.marketFilter {
		marketBlocked = s.marketDropExceeds(barCtx)
	}

	for _, tsCode := range barCtx.Universe {
		// 获取历史K线数据 (自适应模式需要完整K线以计算ATR)
		bars, err := barCtx.History.GetBars(tsCode, barCtx.TradeDate, s.HistoryLen)
		if err != nil || len(bars) < s.minRequiredBars() {
			continue
		}

		// 提取收盘价序列
		closes := make([]float64, len(bars))
		for i, bar := range bars {
			closes[i] = bar.AdjClose()
		}

		// 当前使用的均线周期和仓位
		shortPeriod := s.ShortPeriod
		longPeriod := s.LongPeriod
		positionPct := s.PositionPct

		// 如果启用自适应, 先更新波动率并获取动态参数
		if s.EnableAdaptive && s.adaptive != nil {
			// 提取最高价和最低价序列 (用于ATR计算)
			highs := make([]float64, len(bars))
			lows := make([]float64, len(bars))
			for i, bar := range bars {
				highs[i] = bar.AdjHigh()
				lows[i] = bar.AdjLow()
			}
			s.adaptive.Update(closes, highs, lows)
			shortPeriod, longPeriod = s.adaptive.GetMA()
			positionPct = s.adaptive.GetPositionSize()
		}

		// 确保有足够数据计算均线
		if len(closes) < longPeriod+1 {
			continue
		}

		shortMA := indicator.SMA(closes, shortPeriod)
		longMA := indicator.SMA(closes, longPeriod)

		n := len(closes)
		// 需要今日和昨日的MA值来判断交叉
		if math.IsNaN(shortMA[n-1]) || math.IsNaN(longMA[n-1]) ||
			math.IsNaN(shortMA[n-2]) || math.IsNaN(longMA[n-2]) {
			continue
		}

		prevShort := shortMA[n-2]
		prevLong := longMA[n-2]
		currShort := shortMA[n-1]
		currLong := longMA[n-1]

		// 当前是否持仓
		pos, hasPosition := barCtx.Positions[tsCode]

		// 金叉: 昨日短均线在长均线下方, 今日短均线在长均线上方
		isGoldenCross := prevShort <= prevLong && currShort > currLong
		// 死叉: 昨日短均线在长均线上方, 今日短均线在长均线下方
		isDeathCross := prevShort >= prevLong && currShort < currLong

		if isGoldenCross && !hasPosition {
			// ===== 买入信号过滤层 (金叉) =====
			// 过滤1: 成交量确认 - 当日量需大于过去5日均量的 volConfirmRatio 倍
			if s.volConfirm && !s.volumeConfirmed(bars) {
				logger.L().Debugf("[%s %s] 金叉信号被过滤: 成交量未确认", tsCode, barCtx.TradeDate)
				continue
			}
			// 过滤2: 趋势强度确认 - 短长均线差距需大于阈值
			if s.trendConfirm && !s.trendConfirmed(currShort, currLong) {
				logger.L().Debugf("[%s %s] 金叉信号被过滤: 趋势强度不足", tsCode, barCtx.TradeDate)
				continue
			}
			// 过滤4: 大盘环境过滤 - 大盘当日跌幅超阈值则不买入 (系统性风险)
			if marketBlocked {
				logger.L().Debugf("[%s %s] 金叉信号被过滤: 大盘系统性下跌", tsCode, barCtx.TradeDate)
				continue
			}
			// 过滤3: 冷却期 - 距上次金叉不足 buyCooldownDays 个交易日则跳过
			if s.inCooldown(tsCode, model.DirBuy, bars) {
				logger.L().Debugf("[%s %s] 金叉信号被过滤: 处于买入冷却期", tsCode, barCtx.TradeDate)
				continue
			}

			// 计算买入数量 (按仓位占比, 向下取整到100股)
			bar, ok := barCtx.Bars[tsCode]
			if !ok || bar.AdjClose() <= 0 {
				continue
			}
			targetAmount := barCtx.TotalAsset * positionPct
			qty := int(targetAmount/bar.AdjClose()/100) * 100
			if qty > 0 {
				reason := fmt.Sprintf("均线金叉: MA%d=%.2f上穿MA%d=%.2f", shortPeriod, currShort, longPeriod, currLong)
				if s.EnableAdaptive {
					status := s.adaptive.GetStatus()
					reason = fmt.Sprintf("%s [%s]", reason, status.VolatilityDesc)
				}
				signals = append(signals, model.Signal{
					TsCode:    tsCode,
					Direction: model.DirBuy,
					TargetQty: qty,
					Reason:    reason,
					Strength:  0.8,
				})
				// 记录金叉信号日期, 用于冷却期判断
				s.lastSignalDate[s.signalKey(tsCode, model.DirBuy)] = barCtx.TradeDate
			}
		} else if isDeathCross && hasPosition && pos.TotalQty > 0 {
			// ===== 卖出信号过滤层 (死叉) =====
			// 过滤3: 冷却期 - 距上次死叉不足 sellCooldownDays 个交易日则跳过
			if s.inCooldown(tsCode, model.DirSell, bars) {
				logger.L().Debugf("[%s %s] 死叉信号被过滤: 处于卖出冷却期", tsCode, barCtx.TradeDate)
				continue
			}
			// 卖出全部
			reason := fmt.Sprintf("均线死叉: MA%d=%.2f下穿MA%d=%.2f", shortPeriod, currShort, longPeriod, currLong)
			if s.EnableAdaptive {
				status := s.adaptive.GetStatus()
				reason = fmt.Sprintf("%s [%s]", reason, status.VolatilityDesc)
			}
			signals = append(signals, model.Signal{
				TsCode:    tsCode,
				Direction: model.DirSell,
				TargetQty: pos.TotalQty,
				Reason:    reason,
				Strength:  0.8,
			})
			// 记录死叉信号日期, 用于冷却期判断
			s.lastSignalDate[s.signalKey(tsCode, model.DirSell)] = barCtx.TradeDate
		}
	}

	return signals, nil
}

// minRequiredBars 计算最少需要的历史K线数量
func (s *MACrossStrategy) minRequiredBars() int {
	if s.EnableAdaptive && s.adaptive != nil {
		// 自适应模式: ATR周期(14) + 均线最大可能周期 * 2
		// 实际使用中会动态调整, 这里给一个保守的最小值
		return 40
	}
	return s.LongPeriod + 1
}

// signalKey 生成冷却记录的键, 区分买入/卖出方向
func (s *MACrossStrategy) signalKey(tsCode string, dir int) string {
	if dir == model.DirBuy {
		return tsCode + "|buy"
	}
	return tsCode + "|sell"
}

// tradingDaysSince 计算 lastDate 到当日(含当日)经过的交易日数
// bars 按时间升序排列, 最后一条为当日
// 返回: 0=同日; 正数=经过的交易日数; math.MaxInt32=未在最近K线中找到(视为已超出冷却期)
func tradingDaysSince(bars []model.Bar, lastDate string) int {
	if lastDate == "" {
		return math.MaxInt32
	}
	// 从后往前找, 命中即返回距离
	for i := len(bars) - 1; i >= 0; i-- {
		if bars[i].TradeDate == lastDate {
			return len(bars) - 1 - i
		}
	}
	return math.MaxInt32
}

// inCooldown 判断同方向信号是否处于冷却期 (返回 true 表示应跳过)
// 冷却期N天: 信号产生后第1~N个交易日内不再产生同方向信号, 第N+1个交易日才允许
func (s *MACrossStrategy) inCooldown(tsCode string, dir int, bars []model.Bar) bool {
	last := s.lastSignalDate[s.signalKey(tsCode, dir)]
	if last == "" {
		return false
	}
	cooldown := s.buyCooldownDays
	if dir == model.DirSell {
		cooldown = s.sellCooldownDays
	}
	days := tradingDaysSince(bars, last)
	if days == math.MaxInt32 {
		return false // 已超出记忆窗口, 视为不在冷却期
	}
	return days <= cooldown
}

// volumeConfirmed 成交量确认: 当日成交量 > 过去5日平均成交量 × 倍数
// 数据不足时放行 (避免误杀)
func (s *MACrossStrategy) volumeConfirmed(bars []model.Bar) bool {
	n := len(bars)
	if n < 6 { // 至少需要今日 + 过去5日
		return true
	}
	todayVol := bars[n-1].Vol
	var sum float64
	for i := n - 6; i < n-1; i++ { // 过去5日 (不含今日)
		sum += bars[i].Vol
	}
	avgVol := sum / 5.0
	if avgVol <= 0 {
		return true
	}
	return todayVol > avgVol*s.volConfirmRatio
}

// trendConfirmed 趋势强度确认: (短均线-长均线)/长均线 > 阈值
func (s *MACrossStrategy) trendConfirmed(currShort, currLong float64) bool {
	if currLong <= 0 {
		return false
	}
	return (currShort-currLong)/currLong > s.trendThreshold
}

// marketDropExceeds 大盘环境过滤: 大盘当日跌幅超过阈值返回 true (应拦截买入)
// 无大盘数据时返回 false (放行, 避免因数据缺失误杀)
func (s *MACrossStrategy) marketDropExceeds(barCtx *BarContext) bool {
	idxBars, err := barCtx.History.GetBars(s.marketIndexCode, barCtx.TradeDate, 2)
	if err != nil || len(idxBars) == 0 {
		return false
	}
	today := idxBars[len(idxBars)-1]
	var pct float64
	if today.PctChg != 0 {
		pct = today.PctChg / 100.0 // PctChg 为百分数, 转小数
	} else if today.PreClose > 0 {
		pct = (today.Close - today.PreClose) / today.PreClose
	}
	return pct < s.marketDropThreshold
}
