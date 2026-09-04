// Package model 定义全系统领域模型与纯计算类型。
//
// 硬约束（ARCHITECTURE §11.4）：本包零 internal 依赖，只依赖标准库。
// 金额/价格统一用 Fen（分），数量统一用 Qty（股），业务层禁止用 float64 表示金额。
package model

import (
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
