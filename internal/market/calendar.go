// Package market 交易日推算、涨跌停/停牌/整手/T+1 规则、佣金税费、有效期计算。
//
// 依赖方向（ARCHITECTURE §1.1）：market 只依赖 model，禁止 import store（数据是纯函数，由调用方传入）。
package market

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Loc 全系统唯一时区：Asia/Shanghai (UTC+8)。
// 禁止各处 time.Local 或 Asia/Shanghai 混用（NAS 上 time.Local 可能是 UTC，§11.3）。
var Loc = time.FixedZone("CST", 8*3600)

// parseDate 以 Loc 解析 YYYYMMDD。
func parseDate(date string) (time.Time, error) {
	return time.ParseInLocation("20060102", date, Loc)
}

// CheckDate 外部入口的日期格式闸门：必须正好是 YYYYMMDD 且能落到真实日历日。
//
// 本包的纯函数（QuarterOf / PrevTradeDay 等）按 date[:4] 定长切片，短一个字符就在任务里 panic；
// MCP 的 date/until 与 CLI 的 --date 都必须在进业务前过这一关。
func CheckDate(date string) error {
	if _, err := parseDate(date); err != nil {
		return fmt.Errorf("日期 %q 不是 YYYYMMDD（例：20260904）: %w", date, err)
	}
	return nil
}

// IsTradeDay 交易日判定。日历缺失 → 返回 true（宁可空跑不可整天不动，PRD P0-2）。
func IsTradeDay(cal map[string]bool, date string) bool {
	open, ok := cal[date]
	if !ok {
		return true
	}
	return open
}

// PrevTradeDay 返回 date 之前最近一个交易日（days 为升序交易日列表）。
func PrevTradeDay(days []string, date string) (string, bool) {
	idx := sort.SearchStrings(days, date)
	if idx > 0 {
		return days[idx-1], true
	}
	return "", false
}

// NextTradeDay 返回 date 之后最近一个交易日（days 为升序交易日列表）。
func NextTradeDay(days []string, date string) (string, bool) {
	idx := sort.SearchStrings(days, date)
	if idx < len(days) && days[idx] == date {
		if idx+1 < len(days) {
			return days[idx+1], true
		}
		return "", false
	}
	if idx < len(days) {
		return days[idx], true
	}
	return "", false
}

// quarterBounds 返回指定年月的季度边界（start/end 为 YYYY-MM-DD）。
func quarterBounds(year int, quarter int) (start, end string) {
	startMonth := (quarter-1)*3 + 1
	endMonth := quarter * 3
	start = fmtDate(year, startMonth, 1)
	// end = 季末月最后一天
	endT := time.Date(year, time.Month(endMonth+1), 1, 0, 0, 0, 0, Loc).AddDate(0, 0, -1)
	end = endT.Format("2006-01-02")
	return
}

func fmtDate(y, m, d int) string {
	return strconv.Itoa(y) + "-" + pad2(m) + "-" + pad2(d)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// QuarterOf 返回 date(YYYYMMDD) 所属季度的标签、起始日、结束日。
func QuarterOf(date string) (label, start, end string) {
	y, _ := strconv.Atoi(date[:4])
	m, _ := strconv.Atoi(date[4:6])
	quarter := (m-1)/3 + 1
	start, end = quarterBounds(y, quarter)
	label = strconv.Itoa(y) + "Q" + strconv.Itoa(quarter)
	return
}

// QuarterTradeDays 返回截至 date 的季度内已过交易日数与季度总交易日数。
func QuarterTradeDays(days []string, date string) (elapsed, total int) {
	_, qstart, qend := QuarterOf(date)
	qstartRaw := strings.ReplaceAll(qstart, "-", "")
	qendRaw := strings.ReplaceAll(qend, "-", "")
	for _, d := range days {
		if d >= qstartRaw && d <= qendRaw {
			total++
			if d <= date {
				elapsed++
			}
		}
	}
	return
}

// EODValidUntil EOD 指令有效期 = 下一交易日 15:00。
//
// 算不出下一交易日就是错误，不再退化成"自然日 +1"：那会把有效期落在周六/休市日上，
// 而一张永远执行不了的指令单看起来和正常单没有区别。
func EODValidUntil(days []string, genDate string) (time.Time, error) {
	next, ok := NextTradeDay(days, genDate)
	if !ok {
		return time.Time{}, fmt.Errorf("日历里 %s 之后没有交易日，算不出指令有效期（日历需续拉）", genDate)
	}
	return fifteenOn(next)
}

// fifteenOn 把 YYYYMMDD 拼成当日 15:00（Loc）。
func fifteenOn(date string) (time.Time, error) {
	t, err := parseDate(date)
	if err != nil {
		return time.Time{}, fmt.Errorf("非法交易日期 %q: %w", date, err)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 15, 0, 0, 0, Loc), nil
}
