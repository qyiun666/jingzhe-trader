package screener

import (
	"fmt"
	"strings"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// calcScore 评分: 综合活跃度、资金关注度、估值、趋势、动量
// 权重: 换手20% + 量比20% + 估值15% + 趋势20% + 动量15% + 当日涨跌10%
// 量纲: 仅用于候选排序比较, 分数会随权重调整变化, 禁止下游按绝对值设阈值
func (s *Screener) calcScore(basic model.DailyBasic, pctChg float64, t Trend) float64 {
	// 活跃度: 换手率 (1-10% 最佳, 过高可能是炒作)
	turnoverScore := 0.0
	if basic.TurnoverRate > 0 {
		if basic.TurnoverRate <= 10 {
			turnoverScore = basic.TurnoverRate
		} else {
			turnoverScore = 10 - (basic.TurnoverRate-10)*0.5
		}
	}

	// 资金关注度: 量比 (>1 放量, >5 封顶低分防异常)
	volRatioScore := 0.0
	if basic.VolumeRatio > 0 {
		if basic.VolumeRatio <= 3 {
			volRatioScore = basic.VolumeRatio
		} else if basic.VolumeRatio <= 5 {
			volRatioScore = 3 - (basic.VolumeRatio-3)*0.5
		} else {
			volRatioScore = 1.0 // 异常放量封顶低分
		}
	}

	// 估值吸引力: PE_TTM (10-30 最佳)
	peScore := 0.0
	if basic.PE_TTM > 0 {
		if basic.PE_TTM >= 10 && basic.PE_TTM <= 30 {
			peScore = 3.0
		} else if basic.PE_TTM > 30 && basic.PE_TTM <= 50 {
			peScore = 1.5
		} else if basic.PE_TTM < 10 {
			peScore = 2.0
		}
	}

	// 趋势分: 收盘价高于MA5越多越好 (上限3分)
	trendScore := 0.0
	if t.MA5 > 0 {
		deviation := (basic.Close - t.MA5) / t.MA5 * 100 // 偏离百分比
		if deviation >= 0 && deviation <= 3 {
			trendScore = 2.0 + deviation/3 // 2~3分
		} else if deviation > 3 && deviation <= 8 {
			trendScore = 3.0 - (deviation-3)*0.2 // 3分递减
		} else if deviation > 8 {
			trendScore = 2.0 // 过高也降分
		} else if deviation < 0 {
			trendScore = 1.0 // 低于MA5给低分 (不应到这里, 但防止边界)
		}
	}

	// 动量分: 5日涨幅 (温和上涨最佳, 暴涨递减; 含走平消除 0 分空洞)
	momentumScore := 0.0
	momentum5d := t.Momentum
	if momentum5d >= 0 && momentum5d <= 5 {
		momentumScore = 3.0 // 含走平
	} else if momentum5d > 5 && momentum5d <= 10 {
		momentumScore = 2.0
	} else if momentum5d > 10 {
		momentumScore = 1.0 // 暴涨可能追高
	} else if momentum5d < 0 && momentum5d >= -3 {
		momentumScore = 1.0 // 小幅回调
	} else if momentum5d < -3 {
		momentumScore = 0.0 // 大跌不给分
	}

	// 当日涨跌: 小涨优于大跌
	chgScore := 0.0
	if pctChg > 0 && pctChg <= 5 {
		chgScore = 2.0
	} else if pctChg > 5 && pctChg <= 9 {
		chgScore = 1.0
	} else if pctChg < -3 {
		chgScore = -1.0
	}

	return turnoverScore*0.20 + volRatioScore*0.20 + peScore*0.15 +
		trendScore*0.20 + momentumScore*0.15 + chgScore*0.10
}

// fetchRecentCloses 获取近5个交易日的收盘价 (用于趋势和动量计算)
// 每日期优先读本地 daily_bar, 本地无数据时走 Tushare API; 最多回溯 10 个自然日 (覆盖长假)
func (s *Screener) fetchRecentCloses(today string) map[string][]float64 {
	t, err := time.Parse("20060102", today)
	if err != nil {
		logger.L().Warnw("选股日期解析失败", "date", today, "err", err)
		return nil
	}

	result := make(map[string][]float64)
	collected := 0
	for i := 1; i <= 15 && collected < trendSamples; i++ { // 15 个自然日覆盖春节/国庆长假
		date := t.AddDate(0, 0, -i)
		if date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
			continue
		}
		dateStr := date.Format("20060102")
		bars := s.closesOfDate(dateStr)
		if len(bars) == 0 {
			continue // 非交易日或该日无数据
		}
		for _, bar := range bars {
			result[bar.TsCode] = append(result[bar.TsCode], bar.Close)
		}
		collected++
	}
	return result
}

// closesOfDate 获取某交易日全市场日线: 优先本地 daily_bar, 缺失时走 Tushare API
func (s *Screener) closesOfDate(dateStr string) []model.Bar {
	if s.barRepo != nil {
		if bars, err := s.barRepo.GetBarsByDate(dateStr); err == nil && len(bars) > 0 {
			return bars
		} else if err != nil {
			logger.L().Warnw("查询本地日线失败, 回退 API", "date", dateStr, "err", err)
		}
	}
	bars, err := s.ts.Daily(dateStr)
	if err != nil {
		logger.L().Warnw("拉取历史收盘价失败", "date", dateStr, "err", err)
		return nil
	}
	return bars
}

// trendSamples 判定方向所需的最小样本交易日数 (不含当日)
// 不能放宽: daily_bar 只在 store.StaleKeepRecentDays 窗口内保留全市场日线,
// 超出窗口的日期只剩股票池, 样本会被静默截断成几只票的收盘价
const trendSamples = 5

// Trend 个股近期方向指标; recentCloses 按日期由近及远排列 (index 0 为最近一日)
// 样本不足的字段留 0, 由调用方按"方向未知"处理 —— 不能拿不足样本的均线冒充有效信号
type Trend struct {
	MA5      float64 // 近 5 日均价 (不含当日); 样本不足 5 日为 0
	Momentum float64 // 5 日动量 %: 当日收盘相对 5 个交易日前收盘
	Days     int     // 实际收集到的交易日样本数
}

// calcTrend 由近及远的收盘价序列与当日收盘算出方向指标
func calcTrend(recentCloses []float64, todayClose float64) Trend {
	t := Trend{Days: len(recentCloses)}
	if t.Days == 0 {
		return t
	}
	t.MA5 = avgLatest(recentCloses, trendSamples)
	if oldest := recentCloses[min(t.Days, trendSamples)-1]; oldest > 0 {
		t.Momentum = (todayClose - oldest) / oldest * 100
	}
	return t
}

// avgLatest 序列最近 n 个样本的均值; 样本不足 n 时返回 0 (不返回"部分均值",
// 否则 3 个样本算出的"MA5"会与真正的 MA5 混淆)
func avgLatest(recentCloses []float64, n int) float64 {
	if len(recentCloses) < n {
		return 0
	}
	sum := 0.0
	for _, c := range recentCloses[:n] {
		sum += c
	}
	return sum / float64(n)
}

// buildReason 构建入选理由 (随候选落库, 是候选为何通过方向门槛的唯一留痕)
func (s *Screener) buildReason(basic model.DailyBasic, pctChg float64, t Trend) string {
	var parts []string
	if basic.TurnoverRate >= 3 {
		parts = append(parts, fmt.Sprintf("换手率%.1f%%(活跃)", basic.TurnoverRate))
	}
	if basic.VolumeRatio >= 1.5 {
		parts = append(parts, fmt.Sprintf("量比%.1f(放量)", basic.VolumeRatio))
	}
	if basic.PE_TTM > 0 && basic.PE_TTM <= 30 {
		parts = append(parts, fmt.Sprintf("PE_TTM=%.1f(估值合理)", basic.PE_TTM))
	}
	if basic.PB > 0 && basic.PB <= 2 {
		parts = append(parts, fmt.Sprintf("PB=%.2f(破净或低估值)", basic.PB))
	}
	if pctChg > 0 && pctChg <= 5 {
		parts = append(parts, fmt.Sprintf("涨%.1f%%(温和上涨)", pctChg))
	}
	if basic.CircMV > 0 {
		mvYi := basic.CircMV / 10000 // 万元→亿元
		parts = append(parts, fmt.Sprintf("流通市值%.0f亿", mvYi))
	}
	if t.MA5 > 0 {
		parts = append(parts, fmt.Sprintf("MA%d=%.2f(线上)", t.Days, t.MA5))
	}
	parts = append(parts, fmt.Sprintf("%d日动量%+.1f%%(方向向上)", t.Days, t.Momentum))
	if len(parts) == 0 {
		return "综合评分入选"
	}
	return strings.Join(parts, ", ")
}
