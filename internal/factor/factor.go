package factor

import (
	"context"
	"math"
	"sort"

	"jingzhe-trader/internal/model"
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

// FactorScore 因子得分
type FactorScore struct {
	TsCode string  // 股票代码
	Score  float64 // 标准化后的得分 0-100, 越高越好
	Raw    float64 // 原始因子值
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

	// 复制并排序以计算分位数
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	// 计算 lower 分位数
	lowerVal := quantile(sorted, lower)
	// 计算 upper 分位数
	upperVal := quantile(sorted, upper)

	// 缩尾处理
	result := make([]float64, len(values))
	for i, v := range values {
		if v < lowerVal {
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
// (x - mean) / std
func Standardize(values []float64) []float64 {
	if len(values) == 0 {
		return values
	}

	// 计算均值
	var sum float64
	for _, v := range values {
		sum += v
	}
	mean := sum / float64(len(values))

	// 计算标准差
	var variance float64
	for _, v := range values {
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(len(values))
	std := math.Sqrt(variance)

	// 标准化
	result := make([]float64, len(values))
	if std == 0 {
		// 标准差为0时, 所有值设为0
		for i := range result {
			result[i] = 0
		}
		return result
	}
	for i, v := range values {
		result[i] = (v - mean) / std
	}
	return result
}

// Rank 排名打分 (0-100分, 排名越前分数越高)
// higherBetter 为 true 时, 值越大排名越高; 为 false 时, 值越小排名越高
// 返回 map[索引]score, 调用方需根据索引对应到 tsCode
// 注意: 这里返回的 key 是 values 切片的索引字符串形式, 方便调用方维护对应关系
// 为了与描述一致, 我们使用 float64 索引映射
func Rank(values []float64, higherBetter bool) map[string]float64 {
	n := len(values)
	if n == 0 {
		return map[string]float64{}
	}

	// 创建带索引的切片
	type indexed struct {
		idx   int
		value float64
	}
	items := make([]indexed, n)
	for i, v := range values {
		items[i] = indexed{idx: i, value: v}
	}

	// 排序: higherBetter=true 时降序, false 时升序
	sort.Slice(items, func(i, j int) bool {
		if higherBetter {
			return items[i].value > items[j].value
		}
		return items[i].value < items[j].value
	})

	// 计算排名分数 (0-100)
	// 第1名得100分, 最后1名得0分, 线性分布
	result := make(map[string]float64, n)
	if n == 1 {
		result["0"] = 50.0
		return result
	}

	for rank, item := range items {
		// 排名从0开始, 分数从100线性降到0
		score := 100.0 * (1.0 - float64(rank)/float64(n-1))
		result[itoa(item.idx)] = score
	}
	return result
}

// itoa 简单的整数转字符串, 避免导入 strconv
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
