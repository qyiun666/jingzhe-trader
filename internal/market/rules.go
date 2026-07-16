package market

import (
	"math"
	"time"
)

// A股交易时段规则 (以下时间均以北京时间 Asia/Shanghai UTC+8 为准):
//   - 开盘集合竞价: 9:15 - 9:25
//   - 上午连续竞价: 9:30 - 11:30
//   - 下午连续竞价: 13:00 - 15:00
//   - 收盘集合竞价: 14:57 - 15:00
//
// 入参 t 可能携带任意时区, 函数内部统一转换为北京时间后再判断。

// beijingLocation 北京时区 (Asia/Shanghai)
var beijingLocation = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// 兜底: 直接构造 UTC+8 固定时区
		loc = time.FixedZone("CST", 8*3600)
	}
	return loc
}()

// toBeijing 将任意时区的时间转换为北京时间
func toBeijing(t time.Time) time.Time {
	return t.In(beijingLocation)
}

// beijingDateStr 返回北京时间下的日期字符串 (格式 YYYYMMDD)
// 用于按日期版本化规则 (如 ST 涨跌幅变更), 字符串字典序与日期先后一致
func beijingDateStr(t time.Time) string {
	return t.In(beijingLocation).Format("20060102")
}

// isWeekendBeijing 判断北京时间是否为周末
func isWeekendBeijing(bt time.Time) bool {
	switch bt.Weekday() {
	case time.Saturday, time.Sunday:
		return true
	}
	return false
}

// IsTradingTime 判断是否在 A股交易时段内 (9:30-11:30, 13:00-15:00, 北京时间)
// 注: 该时段包含收盘集合竞价时段 (14:57-15:00)
func IsTradingTime(t time.Time) bool {
	bt := toBeijing(t)
	if isWeekendBeijing(bt) {
		return false
	}
	minutes := bt.Hour()*60 + bt.Minute()

	// 上午连续竞价: 9:30 - 11:30
	if minutes >= 9*60+30 && minutes <= 11*60+30 {
		return true
	}
	// 下午连续竞价: 13:00 - 15:00
	if minutes >= 13*60 && minutes <= 15*60 {
		return true
	}
	return false
}

// IsCallAuction 判断是否在开盘集合竞价时段 (9:15-9:25, 北京时间)
func IsCallAuction(t time.Time) bool {
	bt := toBeijing(t)
	if isWeekendBeijing(bt) {
		return false
	}
	minutes := bt.Hour()*60 + bt.Minute()
	return minutes >= 9*60+15 && minutes <= 9*60+25
}

// IsCloseAuction 判断是否在收盘集合竞价时段 (14:57-15:00, 北京时间)
func IsCloseAuction(t time.Time) bool {
	bt := toBeijing(t)
	if isWeekendBeijing(bt) {
		return false
	}
	minutes := bt.Hour()*60 + bt.Minute()
	return minutes >= 14*60+57 && minutes <= 15*60
}

// RoundLot 买入手数向下取整到 100 股 (A 股 1 手 = 100 股)
// 买入申报数量须为 100 股的整数倍, 不足 100 股部分舍去
func RoundLot(qty int) int {
	if qty <= 0 {
		return 0
	}
	return (qty / 100) * 100
}

// IsvalidLot 判断申报数量是否为有效手数
//   - 买入: 必须为 100 的整数倍
//   - 卖出: 允许零股 (清仓时可卖出不满 100 股的零头)
func IsvalidLot(qty int, isSell bool) bool {
	if qty <= 0 {
		return false
	}
	if isSell {
		// 卖出允许零股 (清仓)
		return true
	}
	// 买入必须为 100 的倍数
	return qty%100 == 0
}

// minPriceTick A 股最小变动价位 (元)
const minPriceTick = 0.01

// RoundPrice 将价格按 A 股最小变动价位 (0.01 元) 四舍五入取整
// A 股最小变动价位为 0.01 元, 涨跌停价计算采用四舍五入规则
func RoundPrice(price float64) float64 {
	if price <= 0 {
		return 0
	}
	// 先放大 100 倍, 四舍五入后再缩小 100 倍
	return math.Round(price/minPriceTick) * minPriceTick
}
