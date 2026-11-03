package screener

import (
	"strings"
	"testing"
)

// TestConfirmStrengthenRequiresConsecutiveDays 定参④：转强要连续 confirmDays 日收盘
// 都站上 MA(window)，单日反弹不算转强；转弱方向由调用方保持即时（本函数只管"能否放开"）。
func TestConfirmStrengthenRequiresConsecutiveDays(t *testing.T) {
	// 最后两日都高于各自 MA：第 4 日 MA(10,10,11)=10.33<11，第 5 日 MA(10,11,12)=11<12。
	strong, note := ConfirmStrengthen([]float64{10, 10, 10, 11, 12}, 3, 2)
	if !strong || note != "" {
		t.Fatalf("连续 2 日站上均线应判转强，实际 strong=%v note=%q", strong, note)
	}
	// 中间掺一天跌破：第 4 日 9 < MA(10,10,9)=9.67 → 确认中断。
	if strong, _ := ConfirmStrengthen([]float64{10, 10, 10, 9, 12}, 3, 2); strong {
		t.Errorf("连续确认被中间一天打断，仍判转强")
	}
	// 只有最后一天站上：第 4 日 10.5 < MA(10,10,10.5)=10.17？——不，10.5 在窗口内会抬高均线。
	// 用明显跌破的第 4 日验证"最后一天合格不等于转强"。
	if strong, _ := ConfirmStrengthen([]float64{12, 12, 12, 8, 20}, 3, 2); strong {
		t.Errorf("倒数第 2 日在均线下，不应判转强")
	}
}

// TestConfirmStrengthenInsufficientHistoryIsNotStrong 数据不够时必须读成"未转强"，
// 不能读成"没有反例所以放行"：这条判据的两种误判代价不对称——晚一天放开仓位只是少赚，
// 早一天放开是把单票上限从 10% 放到 40% 再挨一次下跌。
func TestConfirmStrengthenInsufficientHistoryIsNotStrong(t *testing.T) {
	strong, note := ConfirmStrengthen([]float64{10, 10, 10, 11}, 3, 3) // 需要 3+3-1=5 根
	if strong {
		t.Errorf("只有 4 根数据却判转强")
	}
	if !strings.Contains(note, "按未转强处理") {
		t.Errorf("数据不足要写明兜底方向，实际 %q", note)
	}
	if strong, _ := ConfirmStrengthen(nil, 60, 3); strong {
		t.Errorf("空序列不应判转强")
	}
	// 均线窗口非法（<1）是装配错误，同样兜底为未转强而不是放行。
	if strong, note := ConfirmStrengthen([]float64{10, 10, 10}, 0, 1); strong || note == "" {
		t.Errorf("窗口非法应拒绝转强并给出说明，实际 strong=%v note=%q", strong, note)
	}
}

// TestConfirmStrengthenSingleDayIsLegacy 回退档：confirmDays=0/1 等价于历史行为
// （只看当日收盘对当日均线），这样 screen.gate_confirm_days 设回 1 就是原口径。
func TestConfirmStrengthenSingleDayIsLegacy(t *testing.T) {
	// 收盘 11 ≥ MA(10,10,11)=10.33 → 单日即转强。
	for _, n := range []int{0, 1} {
		strong, note := ConfirmStrengthen([]float64{10, 10, 11}, 3, n)
		if !strong || note != "" {
			t.Errorf("confirmDays=%d 应退化为单日判定，实际 strong=%v note=%q", n, strong, note)
		}
	}
	// 收盘 9 < MA(10,10,9)=9.67 → 单日也不转强。
	if strong, _ := ConfirmStrengthen([]float64{10, 10, 9}, 3, 1); strong {
		t.Errorf("当日在均线下方不应转强")
	}
}

// TestConfirmStrengthenBadCloses 非正收盘（脏数据/停服遗留）一律读成未转强，
// 不能让它把均线摊薄成 0 从而"永远站上"。
func TestConfirmStrengthenBadCloses(t *testing.T) {
	if strong, note := ConfirmStrengthen([]float64{10, 10, 11, 0, 12}, 3, 2); strong || note == "" {
		t.Errorf("窗口内含非正收盘应拒绝转强，实际 strong=%v note=%q", strong, note)
	}
	if strong, _ := ConfirmStrengthen([]float64{10, -1, 11, 12, 13}, 3, 2); strong {
		t.Errorf("负收盘不应判转强")
	}
}

// TestConfirmStrengthenRealMA60Shape 用本轮实盘形状定标：000300 在 09-28~11-02
// 每日收盘都低于 MA60（缺口 -6.0% → -10.45% → -9.55%），连续 3 日确认必须恒判"未转强"。
// 这里用等价的缩短窗口复刻同一种"长期在均线下方缓慢下行的序列"。
func TestConfirmStrengthenRealMA60Shape(t *testing.T) {
	closes := make([]float64, 0, 66)
	for i := 0; i < 66; i++ {
		closes = append(closes, 4600-float64(i)*10) // 缓慢下行：收盘必然低于 MA60
	}
	if strong, _ := ConfirmStrengthen(closes, 60, 3); strong {
		t.Errorf("下行序列不应判转强")
	}
	// 只把最后一天拉到均线上：单日反抽不应转强（防抖要拦的就是这个）。
	last := closes[len(closes)-1]
	closes[len(closes)-1] = 4800
	if strong, _ := ConfirmStrengthen(closes, 60, 3); strong {
		t.Errorf("单日反抽到均线上方不应判转强")
	}
	// 连续 3 日都站上才放行。
	for i := 3; i > 0; i-- {
		closes[len(closes)-i] = 4800 + float64(3-i)
	}
	if strong, note := ConfirmStrengthen(closes, 60, 3); !strong {
		t.Errorf("连续 3 日站上应转强，note=%q（last=%v）", note, last)
	}
}
