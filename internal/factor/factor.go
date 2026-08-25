package factor

import (
	"context"
	"math"
	"sort"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/pkg/logger"
)

// Factor 因子接口
type Factor interface {
	// Name 返回因子名称
	Name() string
	// Compute 在指定截面日期计算股票池的因子值
	// 返回 map[tsCode]因子值, 因子值越高越好
	Compute(ctx context.Context, date string, universe []string, provider DataProvider) (map[string]float64, error)
}

// DataProvider 因子数据提供者
// 因子通过此接口获取所需数据
type DataProvider interface {
	// GetDailyBasic 获取指定交易日的全市场基本面数据
	GetDailyBasic(date string) ([]model.DailyBasic, error)
	// GetDailyBasicByCode 获取指定股票在 [startDate, endDate] 区间内的基本面数据
	GetDailyBasicByCode(tsCode, startDate, endDate string) ([]model.DailyBasic, error)
	// GetFinaIndicator 获取指定股票的全部财务指标
	GetFinaIndicator(tsCode string) ([]model.FinaIndicator, error)
	// GetStockByCode 按代码查询股票基本信息
	GetStockByCode(tsCode string) (*model.Stock, error)
	// GetBars 获取指定股票在 [startDate, endDate] 区间内的日线数据
	GetBars(tsCode, startDate, endDate string) ([]model.Bar, error)
}

// CompositeResult 多因子合成结果
type CompositeResult struct {
	TsCode  string             // 股票代码
	Score   float64            // 综合得分
	Factors map[string]float64 // 各因子原始值
}

// Winsorize 缩尾处理 (去掉极值)
// 小于lower分位数的值替换为lower分位数, 大于upper分位数的值替换为upper分位数
// lower 和 upper 取值范围 [0, 1], 例如 0.05 和 0.95 表示 5% 和 95% 分位数
func Winsorize(values []float64, lower, upper float64) []float64 {
	if len(values) == 0 {
		return values
	}
	if lower < 0 || lower > 1 || upper < 0 || upper > 1 || lower >= upper {
		return values
	}

	// 只用有限值计算分位数 (NaN/Inf 不参与, 原样保留)
	finite := make([]float64, 0, len(values))
	for _, v := range values {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			finite = append(finite, v)
		}
	}
	if len(finite) == 0 {
		result := make([]float64, len(values))
		copy(result, values)
		return result
	}
	sort.Float64s(finite)

	// 计算 lower 分位数
	lowerVal := quantile(finite, lower)
	// 计算 upper 分位数
	upperVal := quantile(finite, upper)

	// 缩尾处理
	result := make([]float64, len(values))
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			result[i] = v // 无效值原样保留, 由下游 Rank 处理
		} else if v < lowerVal {
			result[i] = lowerVal
		} else if v > upperVal {
			result[i] = upperVal
		} else {
			result[i] = v
		}
	}
	return result
}

// quantile 计算分位数 (线性插值法)
// sorted 必须是已排序的数组, p 为分位数 [0, 1]
func quantile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[n-1]
	}

	// 线性插值
	pos := p * float64(n-1)
	lowerIdx := int(math.Floor(pos))
	upperIdx := int(math.Ceil(pos))
	if lowerIdx == upperIdx {
		return sorted[lowerIdx]
	}
	frac := pos - float64(lowerIdx)
	return sorted[lowerIdx] + frac*(sorted[upperIdx]-sorted[lowerIdx])
}

// Standardize 标准化 (z-score)
// (x - mean) / std; 非有限值(NaN/Inf)不参与统计, 输出保留 NaN 供下游识别
func Standardize(values []float64) []float64 {
	if len(values) == 0 {
		return values
	}

	// 只用有限值计算均值/标准差
	var sum float64
	n := 0
	for _, v := range values {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			sum += v
			n++
		}
	}
	result := make([]float64, len(values))
	if n == 0 {
		for i := range result {
			result[i] = math.NaN()
		}
		return result
	}
	mean := sum / float64(n)

	var variance float64
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(n)
	std := math.Sqrt(variance)

	// 标准化
	if std == 0 {
		// 标准差为0时, 有限值设为0, 无效值保持 NaN
		for i, v := range values {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				result[i] = math.NaN()
			} else {
				result[i] = 0
			}
		}
		return result
	}
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			result[i] = math.NaN()
		} else {
			result[i] = (v - mean) / std
		}
	}
	return result
}

// Rank 排名打分 (0-100分, 排名越前分数越高)
// higherBetter 为 true 时, 值越大排名越高; 为 false 时, 值越小排名越高
// 返回与输入等长的切片, 按输入索引对齐 (无效值确定性地给 0 分)
func Rank(values []float64, higherBetter bool) []float64 {
	n := len(values)
	result := make([]float64, n)
	if n == 0 {
		return result
	}

	// 分离有限值与无效值 (NaN/Inf): 无效值确定性地给 0 分
	type indexed struct {
		idx   int
		value float64
	}
	items := make([]indexed, 0, n)
	invalid := 0
	for i, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			invalid++
		} else {
			items = append(items, indexed{idx: i, value: v})
		}
	}
	// 无效值占比过高时告警 (数据质量问题的早期信号)
	if n > 0 && invalid*5 > n {
		logger.L().Warnf("[factor] Rank 输入无效值占比 %d/%d, 排名质量下降", invalid, n)
	}

	// 排序: higherBetter=true 时降序, false 时升序
	sort.Slice(items, func(i, j int) bool {
		if higherBetter {
			return items[i].value > items[j].value
		}
		return items[i].value < items[j].value
	})

	// 计算排名分数 (0-100): 第1名得100分, 最后1名得0分, 线性分布
	m := len(items)
	if m == 0 {
		return result // 全为无效值, 全部 0 分
	}
	if m == 1 {
		result[items[0].idx] = 50.0
		return result
	}
	for rank, item := range items {
		// 排名从0开始, 分数从100线性降到0
		result[item.idx] = 100.0 * (1.0 - float64(rank)/float64(m-1))
	}
	return result
}
