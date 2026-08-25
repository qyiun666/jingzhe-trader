package market

import "sort"

// Calendar 交易日历
// 内部维护一份去重并按日期升序排序的交易日列表 (格式 YYYYMMDD),
// 并构建日期到索引的映射以加速查询。
type Calendar struct {
	tradeDates []string       // 排序后的交易日列表 (YYYYMMDD)
	index      map[string]int // 日期 -> 在 tradeDates 中的索引
}

// NewCalendar 创建交易日历, tradeDates 格式为 YYYYMMDD
// 内部会自动去重并排序
func NewCalendar(tradeDates []string) *Calendar {
	// 去重
	seen := make(map[string]bool, len(tradeDates))
	dates := make([]string, 0, len(tradeDates))
	for _, d := range tradeDates {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		dates = append(dates, d)
	}
	// 按日期升序排序 (YYYYMMDD 字典序与日期先后一致)
	sort.Strings(dates)

	// 建立日期 -> 索引 的映射
	index := make(map[string]int, len(dates))
	for i, d := range dates {
		index[d] = i
	}

	return &Calendar{
		tradeDates: dates,
		index:      index,
	}
}

// IsTradeDate 判断指定日期是否为交易日 (date 格式 YYYYMMDD)
func (c *Calendar) IsTradeDate(date string) bool {
	_, ok := c.index[date]
	return ok
}

// NextTradeDate 返回指定日期之后的下一个交易日 (date 格式 YYYYMMDD)
// 若 date 本身是交易日, 返回其后的下一个交易日 (不包含 date)
// 若不存在下一个交易日, 返回空字符串
func (c *Calendar) NextTradeDate(date string) string {
	// 第一个 > date 的位置 (sort.Search 语义: 最小的使 f(i) 为真的下标)
	i := sort.Search(len(c.tradeDates), func(i int) bool {
		return c.tradeDates[i] > date
	})
	if i < len(c.tradeDates) {
		return c.tradeDates[i]
	}
	return ""
}

// PrevTradeDate 返回指定日期之前的上一个交易日 (date 格式 YYYYMMDD)
// 若 date 本身是交易日, 返回其前的上一个交易日 (不包含 date)
// 若不存在上一个交易日, 返回空字符串
func (c *Calendar) PrevTradeDate(date string) string {
	// 第一个 >= date 的位置, 其前一个即为最后一个 < date 的交易日
	i := sort.Search(len(c.tradeDates), func(i int) bool {
		return c.tradeDates[i] >= date
	})
	if i > 0 {
		return c.tradeDates[i-1]
	}
	return ""
}

// TradeDatesBetween 返回 [start, end] 闭区间内的所有交易日 (格式 YYYYMMDD)
// 若区间无效或无交易日, 返回 nil
func (c *Calendar) TradeDatesBetween(start, end string) []string {
	n := len(c.tradeDates)
	if n == 0 || start > end {
		return nil
	}
	// 第一个 >= start 的位置
	startIdx := sort.Search(n, func(i int) bool {
		return c.tradeDates[i] >= start
	})
	if startIdx >= n {
		return nil
	}
	// 最后一个 <= end 的位置
	endIdx := sort.Search(n, func(i int) bool {
		return c.tradeDates[i] > end
	}) - 1

	if startIdx > endIdx {
		return nil
	}
	result := make([]string, 0, endIdx-startIdx+1)
	result = append(result, c.tradeDates[startIdx:endIdx+1]...)
	return result
}

// IsLastTradeDateOfMonth 判断指定日期是否为当月最后一个交易日 (date 格式 YYYYMMDD)
// 判断逻辑: 若 date 的下一个交易日已属于下一个自然月, 则 date 为当月最后一个交易日
func (c *Calendar) IsLastTradeDateOfMonth(date string) bool {
	if !c.IsTradeDate(date) {
		return false
	}
	next := c.NextTradeDate(date)
	if next == "" {
		// 没有下一个交易日, 视为月末 (同时也是日历中最后一个交易日)
		return true
	}
	// YYYYMMDD 前 6 位为年月, 比较年月是否不同
	if len(date) >= 6 && len(next) >= 6 {
		return date[:6] != next[:6]
	}
	return false
}
