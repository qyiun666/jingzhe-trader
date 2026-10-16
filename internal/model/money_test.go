package model

import "testing"

// TestFromFloatNoFloatError 验证 0.1+0.2 类浮点误差不出现（金额走 int64 分）。
func TestFromFloatNoFloatError(t *testing.T) {
	a := FromFloat(0.1)
	b := FromFloat(0.2)
	sum := a + b
	if sum != 30 {
		t.Fatalf("0.1+0.2 期望 30 分，实际 %d 分（%.2f 元）", int64(sum), sum.Float())
	}
	// 0.3 元应精确为 30 分
	if FromFloat(0.3) != 30 {
		t.Fatalf("FromFloat(0.3) 期望 30，实际 %d", int64(FromFloat(0.3)))
	}
}

// TestFenString 千分位 + 两位小数展示。
func TestFenString(t *testing.T) {
	cases := []struct {
		f    Fen
		want string
	}{
		{168050, "1,680.50"},
		{0, "0.00"},
		{5, "0.05"},
		{-500, "-5.00"},
		{20000000000, "200,000,000.00"}, // 2 亿元
	}
	for _, c := range cases {
		if got := c.f.String(); got != c.want {
			t.Errorf("Fen(%d).String() = %q, 期望 %q", int64(c.f), got, c.want)
		}
	}
}

// TestQtyRoundLot 整手取整正确。
func TestQtyRoundLot(t *testing.T) {
	cases := []struct {
		in   Qty
		down Qty
		up   Qty
	}{
		{150, 100, 200},
		{100, 100, 100},
		{99, 0, 100},
		{200, 200, 200},
		{1, 0, 100},
		{0, 0, 0},
	}
	for _, c := range cases {
		if c.in.RoundLotDown() != c.down {
			t.Errorf("Qty(%d).RoundLotDown() = %d, 期望 %d", int64(c.in), int64(c.in.RoundLotDown()), int64(c.down))
		}
		if c.in.RoundLotUp() != c.up {
			t.Errorf("Qty(%d).RoundLotUp() = %d, 期望 %d", int64(c.in), int64(c.in.RoundLotUp()), int64(c.up))
		}
	}
}

// TestFenMul 单价(分/股) × 股数 → 分。
func TestFenMul(t *testing.T) {
	price := Fen(168050) // 1680.50 元
	total := price.Mul(100)
	if total != 16805000 {
		t.Fatalf("1680.50 × 100 = 期望 16,805,000 分，实际 %d", int64(total))
	}
}

// TestFenPct 百分比计算（四舍五入到分）。
func TestFenPct(t *testing.T) {
	// 100 元 × 8% = 8 元 = 800 分
	if Fen(10000).Pct(0.08) != 800 {
		t.Fatalf("100元×8%% 期望 800 分，实际 %d", int64(Fen(10000).Pct(0.08)))
	}
	// 1680.50 × 8% = 134.44 元 = 13444 分
	if Fen(168050).Pct(0.08) != 13444 {
		t.Fatalf("1680.50×8%% 期望 13444 分，实际 %d", int64(Fen(168050).Pct(0.08)))
	}
}

// TestFenDivOneThird 1/3 分配截取到分（不溢出、不放大）。
func TestFenDivOneThird(t *testing.T) {
	if Fen(100).DivQty(3) != 33 {
		t.Fatalf("100分/3 期望 33 分，实际 %d", int64(Fen(100).DivQty(3)))
	}
}

// TestFenLargeNoOverflow 大额不溢出（int64 分可表示 ±92 亿元）。
func TestFenLargeNoOverflow(t *testing.T) {
	big := FromFloat(200000000.0) // 2 亿元 = 20000000000 分
	if big != 20000000000 {
		t.Fatalf("2亿元 期望 20000000000 分，实际 %d", int64(big))
	}
	// 乘法：100 元/股 × 100 万股 = 1 亿元 = 10000000000 分（仍在 int64 安全范围）
	if Fen(10000).Mul(1000000) != 10000000000 {
		t.Fatalf("大额乘法溢出")
	}
}

// TestCheckTradePriceRange 绝对区间：0 表示"未给出"必须放过（同步常常只给成本不给高点），
// 区间外一律拒绝。用例里的 507 元是 20261015 事故的真实错值（5.07 元被当成分传成 507）。
func TestCheckTradePriceRange(t *testing.T) {
	// 0（含被四舍五入成 0 的极小值）＝"这一项没给"，必须放过：sync_portfolio 的
	// high_price 平时就是省略的，把它当错值拒会让整次同步失败。
	ok := []Fen{0, FromFloat(0.001), MinTradePriceFen, FromFloat(4.85), FromFloat(1700), MaxTradePriceFen}
	for _, v := range ok {
		if err := v.CheckTradePriceRange("cost_price"); err != nil {
			t.Errorf("%s 元本应合法却被拒: %v", v, err)
		}
	}
	bad := []Fen{-1, FromFloat(10000.01), FromFloat(50700)}
	for _, v := range bad {
		if err := v.CheckTradePriceRange("cost_price"); err == nil {
			t.Errorf("%s 元应被区间校验拒绝", v)
		}
	}
}

// TestCheckPriceAgainstRef 相对参照：这道闸专门负责绝对区间放过去的那种错值——
// 507 元是合法价格，但与 4.85 元的收盘差 100 倍，一比就露馅（agent_issues #12-A）。
func TestCheckPriceAgainstRef(t *testing.T) {
	ref := FromFloat(4.85)
	// 真实行情允许的距离：成本远高于现价（高位接盘）、远低于现价（低位起家）都在 8 倍内
	for _, v := range []Fen{FromFloat(5.07), FromFloat(4.85), FromFloat(1.0), FromFloat(30), FromFloat(0)} {
		if err := v.CheckPriceAgainstRef(ref, "cost_price"); err != nil {
			t.Errorf("%s 元对参考价 %s 元本应通过: %v", v, ref, err)
		}
	}
	// 一个数量级以上的偏离＝量纲错，不是行情
	for _, v := range []Fen{FromFloat(507), FromFloat(48500), FromFloat(0.59), FromFloat(-5)} {
		if err := v.CheckPriceAgainstRef(ref, "cost_price"); err == nil {
			t.Errorf("%s 元对参考价 %s 元应被拒（超出 %d 倍）", v, ref, PriceRefMaxRatio)
		}
	}
	// 没有参考价就不臆断：交给绝对区间兜底
	if err := FromFloat(507).CheckPriceAgainstRef(0, "cost_price"); err != nil {
		t.Errorf("ref=0 应跳过相对校验, 实际: %v", err)
	}
}
