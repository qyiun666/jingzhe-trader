package signal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"jingzhe-trader/internal/model"
)

// fen 元 → 分，只走 model.FromFloat 这一个换算口（M4：不得在测试里另造截断/四舍五入口径）。
func fen(yuan float64) model.Fen { return model.FromFloat(yuan) }

// boardRow 一条板块行情：交易所公布的涨停价与跌停价（分）。昨收只在注释里，判定器拿不到它。
// 判定器只见档位价格、不见板块也不见涨跌幅，所以五行必须走同一条算式。
type boardRow struct {
	name     string
	up, down model.Fen
}

func fiveBoards() []boardRow {
	return []boardRow{
		{"主板", fen(11.00), fen(9.00)},   // 昨收 10.00
		{"ST", fen(3.15), fen(2.85)},    // 昨收 3.00
		{"创业板", fen(24.00), fen(16.00)}, // 昨收 20.00
		{"科创板", fen(66.60), fen(44.40)}, // 昨收 55.50
		{"北交所", fen(10.40), fen(5.60)},  // 昨收 8.00
	}
}

// T1/T2/T3/T6：收盘=涨停价 → blocked_limit_up；-1 分 → tradeable；+1 分 → blocked 且记数据异常。
// 五个板块的档位跨度各不相同，判定结果只由「收盘是否触及档位」决定。
func TestJudgeLimitUpAcrossBoards(t *testing.T) {
	for _, b := range fiveBoards() {
		v := JudgeLimitDay(LimitDay{Up: b.up, Down: b.down, RawClose: b.up})
		if v.State != LimitBlockedUp || v.Anomaly {
			t.Errorf("%s 收盘=涨停价: got %+v", b.name, v)
		}
		if !v.Blocks(model.DirBuy) || v.Blocks(model.DirSell) {
			t.Errorf("%s 涨停封板应只拦买入: %+v", b.name, v)
		}

		off := JudgeLimitDay(LimitDay{Up: b.up, Down: b.down, RawClose: b.up - 1})
		if off.State != LimitTradeable || off.Anomaly {
			t.Errorf("%s 涨停价-1 分: got %+v", b.name, off)
		}

		over := JudgeLimitDay(LimitDay{Up: b.up, Down: b.down, RawClose: b.up + 1})
		if over.State != LimitBlockedUp || !over.Anomaly {
			t.Errorf("%s 收盘越过涨停价应 blocked+异常: got %+v", b.name, over)
		}
	}
}

// T7：跌停收盘拦卖出，不拦买入。
func TestJudgeLimitDownBlocksSellOnly(t *testing.T) {
	b := fiveBoards()[0]
	v := JudgeLimitDay(LimitDay{Up: b.up, Down: b.down, RawClose: b.down})
	if v.State != LimitBlockedDown {
		t.Fatalf("got %+v", v)
	}
	if !v.Blocks(model.DirSell) || v.Blocks(model.DirBuy) {
		t.Errorf("跌停封板应只拦卖出: %+v", v)
	}
	if under := JudgeLimitDay(LimitDay{Up: b.up, Down: b.down, RawClose: b.down - 1}); !under.Anomaly {
		t.Errorf("收盘跌破跌停价应记数据异常: %+v", under)
	}
}

// T4/T5：缺行与档位缺失只能落 unknown，且 reason 可区分；缺行由调用方经 UnknownNoRow 构造。
func TestJudgeUnknownReasons(t *testing.T) {
	norow := UnknownNoRow()
	if norow.State != LimitUnknown || norow.Reason != ReasonNoRow {
		t.Errorf("缺行: got %+v", norow)
	}
	// 填 0 冒充有效行会被下面的分支看见——这正是 store 侧必须用缺失键 map 的理由。
	zero := JudgeLimitDay(LimitDay{})
	if zero.State != LimitUnknown || zero.Reason != ReasonLimitLE0 {
		t.Errorf("档位缺失应 unknown/limit_le0: got %+v", zero)
	}
	for _, px := range []LimitDay{
		{Up: fen(11), Down: 0, RawClose: fen(10)},
		{Up: 0, Down: fen(9), RawClose: fen(10)},
		{Up: -1, Down: -2, RawClose: fen(10)},
	} {
		if v := JudgeLimitDay(px); v.State != LimitUnknown || v.Reason != ReasonLimitLE0 {
			t.Errorf("%+v: got %+v", px, v)
		}
	}
	if v := JudgeLimitDay(LimitDay{Up: fen(9), Down: fen(11), RawClose: fen(10)}); v.State != LimitUnknown || v.Reason != ReasonInverted {
		t.Errorf("档位倒挂应 unknown/inverted 不 panic: got %+v", v)
	}
	// 只有前复权价、没有真实成交价时不得给出任何成交性结论（基准价缺失）。
	if v := JudgeLimitDay(LimitDay{Up: fen(11), Down: fen(9)}); v.State != LimitUnknown || v.Reason != ReasonNoPrice {
		t.Errorf("判定基准价缺失应 unknown/no_price: got %+v", v)
	}
}

// T14（判定器半面）：unknown 一律不拦截，数据缺失不得改变买入/卖出链。
func TestUnknownNeverBlocks(t *testing.T) {
	for _, v := range []LimitVerdict{
		UnknownNoRow(),
		JudgeLimitDay(LimitDay{}),
		JudgeLimitDay(LimitDay{Up: fen(9), Down: fen(11), RawClose: fen(10)}),
		JudgeLimitDay(LimitDay{Up: fen(11), Down: fen(9)}),
	} {
		if v.State != LimitUnknown {
			t.Fatalf("期望 unknown: %+v", v)
		}
		if v.Blocks(model.DirBuy) || v.Blocks(model.DirSell) {
			t.Errorf("unknown 不得拦截任何方向: %+v", v)
		}
	}
}

// T8：一字封死与收盘封板必须不同名；high/low 未落库时是第三种形态，不得默认成一字。
func TestJudgeSealStates(t *testing.T) {
	up, down := fen(11.00), fen(9.00)
	oneWord := JudgeLimitDay(LimitDay{Up: up, Down: down, RawClose: up, High: up, Low: up})
	closeOnly := JudgeLimitDay(LimitDay{Up: up, Down: down, RawClose: up, High: up, Low: fen(10.40)})
	unresolved := JudgeLimitDay(LimitDay{Up: up, Down: down, RawClose: up})

	if oneWord.Seal != SealOneWord || closeOnly.Seal != SealCloseOnly || unresolved.Seal != SealUnresolved {
		t.Fatalf("三态形态: one_word=%q close_only=%q unresolved=%q", oneWord.Seal, closeOnly.Seal, unresolved.Seal)
	}
	if oneWord.State != closeOnly.State || oneWord.Reason != closeOnly.Reason {
		t.Errorf("形态不得改写四态与 reason")
	}
	if d := JudgeLimitDay(LimitDay{Up: up, Down: down, RawClose: down, High: down, Low: down}); d.Seal != SealOneWord {
		t.Errorf("一字跌停同样要认出来: %+v", d)
	}
	if ok := JudgeLimitDay(LimitDay{Up: up, Down: down, RawClose: fen(10.50)}); ok.Seal != SealNone {
		t.Errorf("非封板态不应有形态标记: %+v", ok)
	}
}

// 次日回收：一字涨停买不到、一字跌停卖不掉，其余按开盘价可成交；数据缺失只降级不报错。
func TestRecoverT1(t *testing.T) {
	up, down := fen(11.00), fen(9.00)

	buySealed := RecoverT1(model.DirBuy, T1Quote{Open: up, High: up, Low: up, RawClose: up, Up: up, Down: down})
	if buySealed.Fillable || buySealed.Reason != ReasonSealed || buySealed.RetPct != 0 {
		t.Errorf("次日一字涨停买不到: %+v", buySealed)
	}
	sellSealed := RecoverT1(model.DirSell, T1Quote{Open: down, High: down, Low: down, RawClose: down, Up: up, Down: down})
	if sellSealed.Fillable || sellSealed.Reason != ReasonSealed {
		t.Errorf("次日一字跌停卖不掉: %+v", sellSealed)
	}
	// 同一根 K 线，反向就不该被拦。
	if out := RecoverT1(model.DirSell, T1Quote{Open: up, High: up, Low: up, RawClose: up, Up: up, Down: down}); !out.Fillable {
		t.Errorf("一字涨停日出旧单不该被拦: %+v", out)
	}

	// 盘中曾离开档位 ⇒ 可按开盘价成交，理论收益复用 pctChange 口径。
	opened := RecoverT1(model.DirBuy, T1Quote{Open: up, High: up, Low: fen(10.60), RawClose: fen(10.80), Up: up, Down: down})
	if !opened.Fillable || opened.Entry != up || opened.Exit != fen(10.80) {
		t.Fatalf("非一字应按开盘价回收: %+v", opened)
	}
	want := pctChange(fen(10.80).Float(), up.Float())
	if d := opened.RetPct - want; d > 1e-9 || d < -1e-9 {
		t.Errorf("RetPct 必须等于 pctChange 口径: got %v want %v", opened.RetPct, want)
	}

	// high/low 未落库：保守视为可成交，不得凭空判成一字（买卖两个方向都要锁）。
	noHL := RecoverT1(model.DirBuy, T1Quote{Open: up, RawClose: up, Up: up, Down: down})
	if !noHL.Fillable || noHL.Reason != "" {
		t.Errorf("缺 high/low 时买入不判一字: %+v", noHL)
	}
	if noHLSell := RecoverT1(model.DirSell, T1Quote{Open: down, RawClose: fen(9.20), Up: up, Down: down}); !noHLSell.Fillable || noHLSell.Reason != "" {
		t.Errorf("缺 high/low 时卖出不判一字: %+v", noHLSell)
	}

	// 数据失败一律降级（T14 回收半面）：不 panic、不产出可成交结论，且 reason 与判定器同源。
	for _, tc := range []struct {
		q    T1Quote
		want LimitReason
	}{
		{T1Quote{}, ReasonLimitLE0}, // 档位与价格全缺，先报档位
		{T1Quote{Open: up, High: up, Low: up, RawClose: up}, ReasonLimitLE0},                     // 无档位
		{T1Quote{Open: up, High: up, Low: up, Up: up, Down: down}, ReasonNoPrice},                // 无收盘
		{T1Quote{Open: up, High: up, Low: up, RawClose: up, Up: down, Down: up}, ReasonInverted}, // 档位倒挂
	} {
		out := RecoverT1(model.DirBuy, tc.q)
		if out.Fillable || out.RetPct != 0 || out.Reason != tc.want || !out.Anomaly {
			t.Errorf("%+v: got %+v want reason=%q", tc.q, out, tc.want)
		}
	}
	// 开盘越过涨停价但盘中曾低于档位：仍可成交，只留数据异常痕。
	if out := RecoverT1(model.DirBuy, T1Quote{Open: up + 1, High: up + 1, Low: fen(10.50), RawClose: fen(10.80), Up: up, Down: down}); !out.Fillable || !out.Anomaly {
		t.Errorf("开盘越界+盘中离开档位应可成交+异常: %+v", out)
	}
	// 全天最低价也不低于涨停价（哪怕是越界脏数据）⇒ 没有任何档位下可买的机会，仍判买不到。
	if out := RecoverT1(model.DirBuy, T1Quote{Open: up + 1, High: up + 1, Low: up + 1, RawClose: up + 1, Up: up, Down: down}); out.Fillable || out.Reason != ReasonSealed || !out.Anomaly {
		t.Errorf("越界一字仍应判买不到+异常: %+v", out)
	}
}

// T16：回收与判定都是纯函数——同输入重复调用逐字节相同，且不改动入参。
func TestRecoverIsIdempotent(t *testing.T) {
	up, down := fen(11.00), fen(9.00)
	q := T1Quote{Open: fen(10.30), High: up, Low: fen(10.10), RawClose: fen(10.60), Up: up, Down: down}
	px := LimitDay{Up: up, Down: down, RawClose: fen(10.60), High: up, Low: fen(10.10)}

	first := RecoverT1(model.DirBuy, q)
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	vj := JudgeLimitDay(px)
	vJSON, err := json.Marshal(vj)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		again := RecoverT1(model.DirBuy, q)
		b, err := json.Marshal(again)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, firstJSON) || again != first {
			t.Fatalf("第 %d 次回收结果漂移: %s vs %s", i, b, firstJSON)
		}
		av, err := json.Marshal(JudgeLimitDay(px))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(av, vJSON) {
			t.Fatalf("第 %d 次判定结果漂移: %s vs %s", i, av, vJSON)
		}
	}
	if q != (T1Quote{Open: fen(10.30), High: up, Low: fen(10.10), RawClose: fen(10.60), Up: up, Down: down}) {
		t.Errorf("入参被改写")
	}
}

// T17：真实成交价口径在类型层锁死——判定输入里没有前复权 Close 这个字段可填。
func TestJudgeInputHasNoAdjustedCloseField(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(LimitDay{}), reflect.TypeOf(T1Quote{})} {
		var raw, adj bool
		for i := 0; i < typ.NumField(); i++ {
			switch typ.Field(i).Name {
			case "RawClose":
				raw = true
			case "Close":
				adj = true
			}
		}
		if !raw {
			t.Errorf("%s 必须带 RawClose（真实成交价）", typ.Name())
		}
		if adj {
			t.Errorf("%s 不得出现 Close 字段，前复权价不能进判定", typ.Name())
		}
	}
}

// T6 + T18（判定侧半面）：判定区不出现任何涨跌幅阈值，也不引入第二套费用算式。
// 断言对象是证据文件本身，不是复刻的常量表——有人日后塞进 0.098 就会红。
func TestJudgeRegionHasNoPercentageOrFeeRule(t *testing.T) {
	const marker = "涨跌停可成交性判定"
	src, err := os.ReadFile("evidence.go")
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(src, []byte(marker))
	if idx < 0 {
		t.Fatalf("evidence.go 里找不到判定区分隔注释 %q", marker)
	}
	region := src[idx:]
	for _, fn := range []string{"func JudgeLimitDay", "func RecoverT1", "func sealOf", "func limitPairErr"} {
		if !bytes.Contains(region, []byte(fn)) {
			t.Fatalf("判定区扫描范围为空：缺 %s", fn)
		}
	}
	if i := bytes.IndexRune(region, '%'); i >= 0 {
		t.Errorf("判定区出现百分号（涨跌幅阈值嫌疑）：…%s…", snippet(region, i))
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "evidence.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var bad []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || (lit.Kind != token.INT && lit.Kind != token.FLOAT) {
			return true
		}
		if fset.Position(lit.Pos()).Offset < idx || lit.Value == "0" {
			return true
		}
		bad = append(bad, fmt.Sprintf("%s(line %d)", lit.Value, fset.Position(lit.Pos()).Line))
		return true
	})
	if len(bad) > 0 {
		t.Errorf("判定区出现 0 以外的数字字面量，等于私自引入档位规则：%v", bad)
	}
	var refs []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && fset.Position(sel.Pos()).Offset >= idx {
			refs = append(refs, id.Name+"."+sel.Sel.Name)
		}
		return true
	})
	for _, r := range refs {
		if strings.HasPrefix(r, "risk.") {
			t.Errorf("判定区引用了费用算式 %q，摩擦必须由调用方现算", r)
		}
	}
}

func snippet(b []byte, i int) string {
	lo := i - 40
	if lo < 0 {
		lo = 0
	}
	hi := i + 40
	if hi > len(b) {
		hi = len(b)
	}
	return string(b[lo:hi])
}
