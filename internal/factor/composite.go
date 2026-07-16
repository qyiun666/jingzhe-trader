package factor

import (
	"context"
	"math"
	"sort"
	"strconv"
)

// FactorWeight 因子权重配置
type FactorWeight struct {
	Factor Factor  // 因子实例
	Weight float64 // 权重
}

// CompositeFactor 多因子合成策略
type CompositeFactor struct {
	factors []FactorWeight // 因子列表及其权重
	name    string         // 合成因子名称
}

// NewCompositeFactor 创建多因子合成策略
func NewCompositeFactor(name string, factors []FactorWeight) *CompositeFactor {
	return &CompositeFactor{
		name:    name,
		factors: factors,
	}
}

// Name 返回合成因子名称
func (cf *CompositeFactor) Name() string {
	return cf.name
}

// Compute 计算多因子合成得分
// 流程: 对每个因子单独计算 -> 缩尾(5%/95%) -> 标准化 -> 排名打分(0-100) -> 按权重加权求和
func (cf *CompositeFactor) Compute(ctx context.Context, date string, universe []string, provider DataProvider) ([]CompositeResult, error) {
	if len(cf.factors) == 0 {
		return nil, nil
	}

	// 1. 计算所有因子的原始值
	// factorValues[i] 是第 i 个因子的 map[tsCode]rawValue
	factorValues := make([]map[string]float64, len(cf.factors))
	for i, fw := range cf.factors {
		vals, err := fw.Factor.Compute(ctx, date, universe, provider)
		if err != nil {
			return nil, err
		}
		factorValues[i] = vals
	}

	// 2. 找出所有因子都有数据的股票集合 (取交集)
	commonStocks := findCommonStocks(factorValues)
	if len(commonStocks) == 0 {
		return nil, nil
	}

	// 3. 对每个因子进行处理: 缩尾 -> 标准化 -> 排名打分
	// factorScores[i] 是第 i 个因子的 map[tsCode]score (0-100)
	factorScores := make([]map[string]float64, len(cf.factors))
	for i := range cf.factors {
		// 提取该因子在公共股票池中的值, 保持顺序一致
		values := make([]float64, len(commonStocks))
		for j, code := range commonStocks {
			values[j] = factorValues[i][code]
		}

		// 缩尾处理 (5%/95%)
		winsorized := Winsorize(values, 0.05, 0.95)

		// 标准化
		standardized := Standardize(winsorized)

		// 排名打分 (0-100), 越高越好
		rankScores := Rank(standardized, true)

		// 转回 map[tsCode]score
		scoreMap := make(map[string]float64, len(commonStocks))
		for j, code := range commonStocks {
			scoreMap[code] = rankScores[strconv.Itoa(j)]
		}
		factorScores[i] = scoreMap
	}

	// 4. 按权重加权求和
	results := make([]CompositeResult, 0, len(commonStocks))
	totalWeight := 0.0
	for _, fw := range cf.factors {
		totalWeight += math.Abs(fw.Weight)
	}

	for _, code := range commonStocks {
		compositeScore := 0.0
		factorMap := make(map[string]float64, len(cf.factors))

		for i, fw := range cf.factors {
			score := factorScores[i][code]
			factorMap[fw.Factor.Name()] = factorValues[i][code] // 保存原始因子值
			compositeScore += score * fw.Weight
		}

		// 归一化到 0-100
		if totalWeight > 0 {
			compositeScore = compositeScore / totalWeight * 100
		}

		results = append(results, CompositeResult{
			TsCode:  code,
			Score:   compositeScore,
			Factors: factorMap,
		})
	}

	// 按综合得分降序排序
	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	return results, nil
}

// SelectTopN 选取得分最高的N只股票
func SelectTopN(results []CompositeResult, n int) []CompositeResult {
	if n <= 0 || len(results) == 0 {
		return nil
	}
	if n >= len(results) {
		// 返回副本
		dst := make([]CompositeResult, len(results))
		copy(dst, results)
		return dst
	}
	// 返回前N个的副本
	dst := make([]CompositeResult, n)
	copy(dst, results[:n])
	return dst
}

// findCommonStocks 找出所有因子都有数据的股票 (取交集)
func findCommonStocks(factorValues []map[string]float64) []string {
	if len(factorValues) == 0 {
		return nil
	}

	// 以第一个因子的股票集合为基础
	base := factorValues[0]
	common := make([]string, 0, len(base))

	for code := range base {
		// 检查该股票是否在所有因子中都存在
		allPresent := true
		for i := 1; i < len(factorValues); i++ {
			if _, ok := factorValues[i][code]; !ok {
				allPresent = false
				break
			}
		}
		if allPresent {
			common = append(common, code)
		}
	}

	// 排序以保证结果稳定
	sort.Strings(common)
	return common
}
