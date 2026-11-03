package screener

import "fmt"

// 大盘"转强"的防抖判据（2026-11-03 组长定参④）。
//
// 为什么需要它：弱势试探档的仓位收缩（总仓≤20%、单票≤10%）与买入闸都挂在
// "指数收盘 < MA60"这一条判据上。单日收盘上穿 MA60 就立刻放回常规档
// （DefaultParams：单票 40%、总仓 90%），意味着一次反抽就能把仓位上限放大 4 倍——
// 而 20261103 复盘的全部亏损都来自"弱势里硬做小额试探"，一旦反抽失败，
// 同样一次上穿在常规档下的代价是它的 4 倍。
//
// 不对称原则：转强慢（连续 confirmDays 日确认）、转弱快（当日跌破即收缩）。
// 入场侧多等一天的成本是"少赚"，出场侧多等一天的成本是"多亏"，两者不同量级。

// ConfirmStrengthen 判断"最近 confirmDays 个交易日是否每日收盘都 ≥ 当日 MA(window)"。
//
// closes 必须按交易日**升序**给出（最后一个元素是当日）。判定逐日回溯：第 i 日
// 的均线用该日及其前 window-1 日的收盘算术平均，因此至少需要 window+confirmDays-1
// 根数据；不足时 strong=false 并给出 note——"数据不够"永远读成"没转强"，
// 因为这条判据的两种误判代价不对称：晚一天放开仓位只是少赚，早一天放开是把
// 仓位上限放到 4 倍再挨一次下跌。
//
// confirmDays ≤ 1 时退化为"当日收盘 ≥ 当日 MA"，与未加防抖的历史行为一致
// （config screen.gate_confirm_days 设 0 或 1 就是这个回退档）。
func ConfirmStrengthen(closes []float64, window, confirmDays int) (strong bool, note string) {
	if window < 1 {
		return false, fmt.Sprintf("均线窗口 %d 非法（须 ≥1）", window)
	}
	if confirmDays < 1 {
		confirmDays = 1
	}
	need := window + confirmDays - 1
	if len(closes) < need {
		return false, fmt.Sprintf("指数日线只有 %d 根，算不出连续 %d 日的 MA%d（需 ≥%d 根），按未转强处理",
			len(closes), confirmDays, window, need)
	}
	for k := len(closes) - confirmDays; k < len(closes); k++ {
		if closes[k] <= 0 {
			return false, fmt.Sprintf("第 %d 根收盘非正（%v），按未转强处理", k+1, closes[k])
		}
		var sum float64
		for j := k - window + 1; j <= k; j++ {
			if closes[j] <= 0 {
				return false, fmt.Sprintf("MA%d 窗口内含非正收盘，按未转强处理", window)
			}
			sum += closes[j]
		}
		ma := sum / float64(window)
		if closes[k] < ma {
			return false, fmt.Sprintf("近 %d 日中有 1 日收盘低于 MA%.2f，未达连续 %d 日确认",
				confirmDays, ma, confirmDays)
		}
	}
	return true, ""
}
