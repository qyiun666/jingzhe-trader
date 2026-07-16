package market

import (
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
)

// 涨跌停规则 (按板块与日期版本化)
//
// 重要历史变更:
//   - 2026-07-06 起, 沪深主板 ST/*ST 股票涨跌幅限制由 ±5% 放宽至 ±10%
//
// 当前各板块涨跌幅限制:
//   - 科创板 / 创业板: ±20%
//   - 北交所:          ±30%
//   - 沪深主板:        ±10% (ST 股 2026-07-06 前为 ±5%)

// stLimitExpandDateStr ST 涨跌幅放宽生效日期 (北京时间, YYYYMMDD 格式)
// 该日期 (含) 起, 主板 ST 股涨跌幅由 5% 放宽至 10%
const stLimitExpandDateStr = "20260706"

// 涨跌幅限制常量
const (
	limitPctMain    = 0.10 // 主板 10%
	limitPctST      = 0.05 // 主板 ST (放宽前) 5%
	limitPctGEMSTAR = 0.20 // 创业板/科创板 20%
	limitPctBSE     = 0.30 // 北交所 30%
)

// PriceLimitPct 根据板块、是否 ST、日期返回涨跌幅限制 (返回值如 0.10 表示 10%)
//
// 规则:
//   - 科创板 / 创业板: ±20%
//   - 北交所:          ±30%
//   - 沪深主板:        ±10% (ST 股 2026-07-06 前为 ±5%)
func PriceLimitPct(tsCode string, isST bool, date time.Time) float64 {
	board := model.DetectBoard(tsCode)
	switch board {
	case model.BoardSTAR, model.BoardChiNext:
		// 科创板 / 创业板: 20%
		return limitPctGEMSTAR
	case model.BoardBSE:
		// 北交所: 30%
		return limitPctBSE
	case model.BoardMainSH, model.BoardMainSZ:
		// 沪深主板: 默认 10%, ST 股在 2026-07-06 前为 5%
		if isST && beijingDateStr(date) < stLimitExpandDateStr {
			return limitPctST
		}
		return limitPctMain
	default:
		// 未知板块, 按主板规则兜底处理
		if isST && beijingDateStr(date) < stLimitExpandDateStr {
			return limitPctST
		}
		return limitPctMain
	}
}

// CalcUpLimit 计算涨停价 (自算兜底, 优先使用 Tushare stk_limit 的精确值)
// 计算公式: 涨停价 = RoundPrice(preClose × (1 + limitPct))
func CalcUpLimit(preClose float64, tsCode string, isST bool, date time.Time) float64 {
	if preClose <= 0 {
		return 0
	}
	pct := PriceLimitPct(tsCode, isST, date)
	return RoundPrice(preClose * (1 + pct))
}

// CalcDownLimit 计算跌停价 (自算兜底)
// 计算公式: 跌停价 = RoundPrice(preClose × (1 - limitPct))
func CalcDownLimit(preClose float64, tsCode string, isST bool, date time.Time) float64 {
	if preClose <= 0 {
		return 0
	}
	pct := PriceLimitPct(tsCode, isST, date)
	return RoundPrice(preClose * (1 - pct))
}

// 价格比较容差, 用于吸收浮点误差 (远小于最小变动价位 0.01)
const priceEpsilon = 1e-6

// IsUpLimit 判断收盘价是否涨停 (收盘价 >= 涨停价)
func IsUpLimit(close, upLimit float64) bool {
	if upLimit <= 0 {
		return false
	}
	return close >= upLimit-priceEpsilon
}

// IsDownLimit 判断收盘价是否跌停 (收盘价 <= 跌停价)
func IsDownLimit(close, downLimit float64) bool {
	if downLimit <= 0 {
		return false
	}
	return close <= downLimit+priceEpsilon
}

// CheckLimit 检查订单是否触发涨跌停限制
// 规则: 涨停时不能买入, 跌停时不能卖出
//   - 买入: 申报价 >= 涨停价时拒绝 (涨停封板无法买入)
//   - 卖出: 申报价 <= 跌停价时拒绝 (跌停封板无法卖出)
//   - upLimit/downLimit <= 0 时视为无限制信息, 不拦截
func CheckLimit(side model.Side, price, upLimit, downLimit float64) error {
	if upLimit <= 0 || downLimit <= 0 {
		// 缺少涨跌停价信息, 不做拦截
		return nil
	}
	switch side {
	case model.SideBuy:
		// 涨停不能买入 (申报价达到或超过涨停价)
		if price >= upLimit-priceEpsilon {
			return fmt.Errorf("涨停限制: 无法以涨停价 %.2f 买入", upLimit)
		}
	case model.SideSell:
		// 跌停不能卖出 (申报价达到或低于跌停价)
		if price <= downLimit+priceEpsilon {
			return fmt.Errorf("跌停限制: 无法以跌停价 %.2f 卖出", downLimit)
		}
	}
	return nil
}
