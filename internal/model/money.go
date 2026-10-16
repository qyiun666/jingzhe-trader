// Package model 定义全系统领域模型与纯计算类型。
//
// 硬约束（ARCHITECTURE §11.4）：本包零 internal 依赖，只依赖标准库。
// 金额/价格统一用 Fen（分），数量统一用 Qty（股），业务层禁止用 float64 表示金额。
package model

import (
	"fmt"
	"strconv"
	"strings"
)

// fenPerYuan 1 元 = 100 分。
const fenPerYuan = 100

// Fen 金额/价格单位：分（1 元 = 100 分）。全系统金额与价格的唯一表示。
type Fen int64

// Qty 数量单位：股。
type Qty int64

// FromFloat 元 → 分，四舍五入到最近整数（half away from zero）。
// 用于两处合法转换：Tushare 解码、配置读取用户输入的元值。
func FromFloat(yuan float64) Fen {
	return Fen(roundFloat(yuan * fenPerYuan))
}

// Float 分 → 元（仅用于展示与回传给 Tushare）。
func (f Fen) Float() float64 {
	return float64(f) / fenPerYuan
}

// Mul 金额 = 单价(分/股) × 股数 → 分。
func (f Fen) Mul(q Qty) Fen {
	return Fen(int64(f) * int64(q))
}

// Pct 返回 f × r，四舍五入到分。r 为比例（0.08 表示 8%）。
func (f Fen) Pct(r float64) Fen {
	return FromFloat(f.Float() * r)
}

// DivQty 分 / 股，向零取整（用于加权成本近似）。
func (f Fen) DivQty(q Qty) Fen {
	if q == 0 {
		return 0
	}
	return Fen(int64(f) / int64(q))
}

// Add 金额相加。
func (f Fen) Add(o Fen) Fen { return f + o }

// Sub 金额相减。
func (f Fen) Sub(o Fen) Fen { return f - o }

// String 以千分位 + 两位小数的形式展示金额，如 "1,680.50"。
func (f Fen) String() string {
	neg := ""
	v := int64(f)
	if v < 0 {
		neg = "-"
		v = -v
	}
	yuan := v / fenPerYuan
	cents := v % fenPerYuan
	intStr := groupThousands(strconv.FormatInt(yuan, 10))
	return neg + intStr + "." + pad2(int(cents))
}

// IsZero 判断是否为零金额。
func (f Fen) IsZero() bool { return f == 0 }

// A 股价格的合理区间（分）：0.01 元 ~ 10000 元。
//
// 下限取到最小面值之下仍留余量（0.2 元面值的 *ST 折算后也在 1 分以上）；上限留出一个
// 数量级——千元级只有极少数票。它挡的是数量级写错的价格，不是"太贵的好股票"。
const (
	MinTradePriceFen Fen = 1
	MaxTradePriceFen Fen = 1000000
)

// PriceRefMaxRatio 价格相对参考价的允许偏离倍数（两侧对称：不超过 ref×，不低于 ref÷）。
//
// 取 8 而不是 2：真实持仓的成本可以离现价很远（高位接盘后被腰斩、低位建仓后翻几倍），
// 但**没有**离到 8 倍以上——那是量纲错误的特征，不是行情的特征。
const PriceRefMaxRatio = 8

// CheckTradePriceRange 校验价格在 A 股合理区间内。0 视为"未给出"，由调用方决定默认值。
//
// 对外入参的量纲约定是"元"（库里存"分"），换算点错一次就是 100 倍：
// agent_issues #12-A（20261015）里 5.07 元的成本被写成 507，止损线随之被算成 466 元，
// 系统每次都判"已跌破止损"，直接误发 M3 紧急邮件惊扰用户。
func (f Fen) CheckTradePriceRange(label string) error {
	if f == 0 {
		return nil
	}
	if f < MinTradePriceFen || f > MaxTradePriceFen {
		return fmt.Errorf("%s=%s 元超出 A 股合理价格区间（%s~%s 元）",
			label, f, MinTradePriceFen, MaxTradePriceFen)
	}
	return nil
}

// CheckPriceAgainstRef 校验价格与参考价（同一只票最近可用收盘价 / 指令单参考价）没有差出
// 一个数量级。ref<=0（没有可用参考价）时跳过——没有基准就不做臆断，区间校验仍然生效。
//
// 这道闸比绝对区间更关键：0.01~10000 元挡不住"5.07 元写成 507 元"（507 元本身是合法价格），
// 而它相对 4.85 元的收盘差了 100 倍，在这里必须被拦下。
func (f Fen) CheckPriceAgainstRef(ref Fen, label string) error {
	if f == 0 || ref <= 0 {
		return nil
	}
	hi, lo := ref*Fen(PriceRefMaxRatio), ref/Fen(PriceRefMaxRatio)
	if f > hi || f < lo {
		return fmt.Errorf("%s=%s 元与参考价 %s 元相差超过 %d 倍（疑似把\"分\"当\"元\"或反之），"+
			"请先与券商成交记录核对单位", label, f, ref, PriceRefMaxRatio)
	}
	return nil
}

// LotShares A 股一手 = 100 股（整手约束的唯一出处）。
const LotShares = 100

// RoundLotDown 向下取整到一手。
func (q Qty) RoundLotDown() Qty {
	return (q / LotShares) * LotShares
}

// RoundLotUp 向上取整到一手。
func (q Qty) RoundLotUp() Qty {
	if q%LotShares == 0 {
		return q
	}
	return (q/LotShares + 1) * LotShares
}

// Add 数量相加。
func (q Qty) Add(o Qty) Qty { return q + o }

// Sub 数量相减（不保证非负，调用方自行校验）。
func (q Qty) Sub(o Qty) Qty { return q - o }

// String 以字符串形式展示股数。
func (q Qty) String() string {
	return strconv.FormatInt(int64(q), 10)
}

// roundFloat 四舍五入到最近整数（half away from zero），避免 0.1+0.2 类浮点误差。
func roundFloat(x float64) int64 {
	if x >= 0 {
		return int64(x + 0.5)
	}
	return int64(x - 0.5)
}

// pad2 补零到两位。
func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// groupThousands 为整数部分插入千分位逗号。
func groupThousands(s string) string {
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	rem := len(s) % 3
	if rem == 0 {
		rem = 3
	}
	b.WriteString(s[:rem])
	for i := rem; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
