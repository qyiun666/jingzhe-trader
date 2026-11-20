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
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

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

// T6 + T18（判定侧半面）：判定算式里不得出现任何涨跌幅阈值，也不得引用费用算式。
// 断言对象是证据文件本身，不是复刻的常量表——有人日后塞进 0.098 就会红。
//
// 作用域刻意分成三条，因为一把梭扫整个判定区会自欺：
//  1. 判定函数体内除 0 以外不许有任何数字字面量 ⇒ 阈值只能来自入参档位；
//  2. 判定区的顶层 const/var 不许有浮点字面量 ⇒ 9.8 / 0.098 这类写法立刻红，
//     而摘要预算 limitBriefTop=10、limitBriefMax=600 是整数、合法且必要；
//  3. 判定区的注释里不许出现「数字+百分号」⇒ 注释留数字等于给后人递阈值
//     （本区原注释写过「主板 10%、ST 5%」，就是被这条规则逼着删掉的）。
//     注意不能退化成"整区不许出现 %"——Sprintf 的格式动词满是 %，那样只能假绿。
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

	judged := map[string]bool{
		"JudgeLimitDay": true, "RecoverT1": true, "sealOf": true,
		"limitPairErr": true, "Blocks": true, "UnknownNoRow": true, "limitEntry": true,
	}
	// 用 ParseComments 一次解析，四条规则共用同一棵语法树（分两次解析会留下两份真相）。
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "evidence.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	inRegion := func(p token.Pos) bool { return fset.Position(p).Offset >= idx }

	for _, cg := range file.Comments {
		if !inRegion(cg.Pos()) {
			continue
		}
		if m := regexp.MustCompile(`[0-9]+(\.[0-9]+)?\s*%`).FindString(cg.Text()); m != "" {
			t.Errorf("判定区注释出现「数字+百分号」%q（line %d）：阈值不该写进判定侧文档", m, fset.Position(cg.Pos()).Line)
		}
	}

	var badBody, badConst []string
	for _, d := range file.Decls {
		switch decl := d.(type) {
		case *ast.FuncDecl:
			if !judged[decl.Name.Name] {
				continue
			}
			delete(judged, decl.Name.Name)
			ast.Inspect(decl.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || (lit.Kind != token.INT && lit.Kind != token.FLOAT) {
					return true
				}
				if lit.Value != "0" {
					badBody = append(badBody, fmt.Sprintf("%s 体内 %s(line %d)", decl.Name.Name, lit.Value, fset.Position(lit.Pos()).Line))
				}
				return true
			})
		case *ast.GenDecl:
			if (decl.Tok != token.CONST && decl.Tok != token.VAR) || !inRegion(decl.Pos()) {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if ok && lit.Kind == token.FLOAT {
					badConst = append(badConst, fmt.Sprintf("%s(line %d)", lit.Value, fset.Position(lit.Pos()).Line))
				}
				return true
			})
		}
	}
	if len(judged) > 0 {
		t.Fatalf("判定函数清单里有名字不在文件里（规则已过期或函数被改名）：%v", keysOf(judged))
	}
	if len(badBody) > 0 {
		t.Errorf("判定函数体里出现 0 以外的数字字面量，等于私自引入档位规则：%v", badBody)
	}
	if len(badConst) > 0 {
		t.Errorf("判定区顶层常量出现浮点字面量（涨跌幅阈值形态）：%v", badConst)
	}

	// 判定区不引用费用算式：摩擦一律由调用方用 risk.RoundTripCostFenFor 现算（T18 判定侧半面）。
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !inRegion(sel.Pos()) {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "risk" {
			t.Errorf("判定区引用了费用算式 risk.%s（line %d），摩擦必须由调用方现算", sel.Sel.Name, fset.Position(sel.Pos()).Line)
		}
		return true
	})
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 硬要求【2】：无条件观测下"算过"必须留下句子，0 拦截也不能返回空串——
// omitempty 对空串同样省略，返回空串就等于把「算过且 0 拦截」和「旧二进制没接线」压回同一态。
func TestLimitBriefAlwaysNonEmptyWhenComputed(t *testing.T) {
	up, down := fen(11.00), fen(9.00)
	rows := map[string]LimitDay{
		"600001": {Up: up, Down: down, RawClose: fen(10.50)},
		"600002": {Up: up, Down: down, RawClose: fen(10.00)},
	}
	got := LimitBrief([]string{"600001", "600002"}, rows)
	if got == "" {
		t.Fatal("全可成交时返回了空串 ⇒ 载荷键消失，与未接线同形")
	}
	if !strings.HasPrefix(got, "涨跌停影子 0/候选 2 只") {
		t.Errorf("0 拦截句前缀漂移（前缀必须与有清单时同型，一句 grep 捞两种日子）：%q", got)
	}
	if !strings.Contains(got, "判定已生效") {
		t.Errorf("0 拦截句必须自证判定跑过：%q", got)
	}
	// 候选集本身为空（漏斗筛空/关闸日）同样是"算过"，不得退化成空串。
	if empty := LimitBrief(nil, nil); !strings.HasPrefix(empty, "涨跌停影子 0/候选 0 只") {
		t.Errorf("空候选集句=%q，期望仍出句子", empty)
	}
}

// 格式锁：整串逐字节钉死（照 TestSectorShadowBriefFormatLockNonEmpty 的先例）。
// 载荷字面量必须直接用四态/形态/reason 常量，翻成中文等于再造一份口径。
func TestLimitBriefFormatLock(t *testing.T) {
	rows := map[string]LimitDay{
		"600001": {Up: fen(11.00), Down: fen(9.00), RawClose: fen(11.00), High: fen(11.00), Low: fen(11.00)},
		"600002": {Up: fen(11.00), Down: fen(9.00), RawClose: fen(9.00)},
		"600003": {Up: fen(11.00), Down: fen(9.00), RawClose: fen(10.00)},
	}
	want := "涨跌停影子 2/候选 3 只（拦买 1、拦卖 1、判定未知 0；档位取交易所公布涨跌停价，" +
		"价格基准=真实收盘价，纯观测不参与决策）：" +
		"600001 blocked_limit_up/one_word 11.00、600002 blocked_limit_down/unresolved 9.00。"
	if got := LimitBrief([]string{"600001", "600002", "600003"}, rows); got != want {
		t.Errorf("格式漂移:\n got=%q\nwant=%q", got, want)
	}
}

// 缺失键 map 的语义必须与"有行但档位为 0"可区分：前者 no_row（接口没供数/库里没这行），
// 后者 limit_le0（有行没档位）。两者都算 unknown，但归因结论完全不同。
func TestLimitBriefDistinguishesNoRowFromNoLimit(t *testing.T) {
	rows := map[string]LimitDay{
		"600001": {}, // 有行没档位（工程若填 0 冒充就会落这里）
	}
	got := LimitBrief([]string{"600001", "600002"}, rows)
	if !strings.Contains(got, "600001 unknown/limit_le0") || !strings.Contains(got, "600002 unknown/no_row") {
		t.Errorf("两种 unknown 未区分：%q", got)
	}
	if !strings.Contains(got, "判定未知 2") || !strings.HasPrefix(got, "涨跌停影子 2/候选 2 只") {
		t.Errorf("计数不对：%q", got)
	}
	// rows 整体为 nil（接线漏了）必须一眼看出来：全部 no_row，而不是静默出空串。
	if allMissing := LimitBrief([]string{"600001", "600002"}, nil); !strings.Contains(allMissing, "拦买 0、拦卖 0、判定未知 2") {
		t.Errorf("rows=nil 时应全记 no_row：%q", allMissing)
	}
}

// 裁定【5】：最多列前 10 只、总数永远精确、整段 rune 数永远 ≤600。
func TestLimitBriefTopTenAndLengthBound(t *testing.T) {
	up, down := fen(11.00), fen(9.00)
	var codes []string
	rows := map[string]LimitDay{}
	for i := 0; i < 25; i++ {
		code := fmt.Sprintf("60%04d", i)
		codes = append(codes, code)
		rows[code] = LimitDay{Up: up, Down: down, RawClose: up}
	}
	got := LimitBrief(codes, rows)
	if !strings.HasPrefix(got, "涨跌停影子 25/候选 25 只") {
		t.Errorf("总数被截断改了：%q", got[:40])
	}
	if !strings.Contains(got, "列前 10") {
		t.Errorf("未标注实列只数：%q", got)
	}
	if n := utf8.RuneCountInString(got); n > 600 {
		t.Errorf("%d 字符，超过裁定【5】的 600 上限", n)
	}
	// 10 与 600 在这里刻意写死：引用 limitBriefTop/limitBriefMax 会让断言跟着常量一起漂，
	// 把"改掉预算"这种语义变更判成绿灯。
	if strings.Count(got, "blocked_limit_up") != 10 {
		t.Errorf("条目数不是裁定【5】的 10：%d", strings.Count(got, "blocked_limit_up"))
	}

	// 条目长得连一只都放不下：兜底句仍须报全总数与四档计数，且不得半条记录。
	long := make([]string, 0, 3)
	longRows := map[string]LimitDay{}
	for i := 0; i < 3; i++ {
		code := strings.Repeat("长", 600) + fmt.Sprintf("%d", i)
		long = append(long, code)
		longRows[code] = LimitDay{Up: up, Down: down, RawClose: up}
	}
	fallback := LimitBrief(long, longRows)
	if !strings.Contains(fallback, "涨跌停影子 3/候选 3 只") || !strings.Contains(fallback, "条目过长只报总数") {
		t.Errorf("兜底句不对：%q", fallback[:80])
	}
	if n := utf8.RuneCountInString(fallback); n > 600 {
		t.Errorf("兜底句 %d 字符，超过裁定【5】的 600 上限", n)
	}
	if strings.Contains(fallback, "blocked_limit_up") {
		t.Errorf("兜底句里不得留半条记录：%q", fallback)
	}
	// n=0 表示「不列条目」，此时"列前 0"是无意义句：只报总数的兜底与 0 拦截句都不许出现。
	if strings.Contains(fallback, "列前") {
		t.Errorf("兜底句不得提列前只数：%q", fallback)
	}
}

// 截断必须按 600 上界收缩「列前」只数，而不是按「反正最多 10 只」硬塞。
//
// 条目用 32 字符代码撑长（stress fixture：真实 ts_code 只有 9 字符，生产形态下这条分支极少
// 触发；留着是因为它一旦触发就是「句子撑爆 detail」）。单条 = 32 代码 + 1 + 16 状态 + 1 + 10
// 形态（high/low 未落库 ⇒ unresolved）+ 1 + 5 价格 = 66 字符，整段 = 74 头部 + 66n + (n-1) 顿号
// + 1 句号：n=8 ⇒ 610（>600 被拒），n=7 ⇒ 543 ⇒ 实列 7 是手算钉死的。
// 有人把预算抬到 700 时 n=8 就合法了，这一枪当场变红（M-M）。写死 7 而不是引用 limitBriefMax，
// 同 TestLimitBriefTopTenAndLengthBound 里写死 10/600 的理由一致。
func TestLimitBriefTruncatesEntriesToRuneBudget(t *testing.T) {
	up, down := fen(11.00), fen(9.00)
	var codes []string
	rows := map[string]LimitDay{}
	for i := 0; i < 12; i++ {
		code := strings.Repeat("长", 30) + fmt.Sprintf("%02d", i)
		codes = append(codes, code)
		rows[code] = LimitDay{Up: up, Down: down, RawClose: up}
	}
	got := LimitBrief(codes, rows)
	if n := strings.Count(got, "blocked_limit_up"); n != 7 {
		t.Errorf("列出条数 = %d，期望 7（第 8 条会把整段推到 610 字符）：%d 字符", n, utf8.RuneCountInString(got))
	}
	if !strings.Contains(got, "列前 7") {
		t.Errorf("实列只数没诚实报出来：%q", got)
	}
	if !strings.HasPrefix(got, "涨跌停影子 12/候选 12 只") {
		t.Errorf("截断日总数句必须仍是精确 12：%q", got[:40])
	}
	if utf8.RuneCountInString(got) > 600 {
		t.Errorf("整段 %d 字符超过 600", utf8.RuneCountInString(got))
	}
}

// T16 同型：摘要也是纯函数，同输入必得逐字节相同的串。
func TestLimitBriefIsDeterministic(t *testing.T) {
	up, down := fen(11.00), fen(9.00)
	codes := []string{"600001", "600002", "000001", "301999"}
	rows := map[string]LimitDay{
		"600001": {Up: up, Down: down, RawClose: up, High: up, Low: down},
		"600002": {Up: up, Down: down, RawClose: down, High: down, Low: down},
		"000001": {Up: up, Down: down, RawClose: fen(10.00)},
	}
	want := LimitBrief(codes, rows)
	for i := 0; i < 16; i++ {
		if got := LimitBrief(codes, rows); got != want {
			t.Fatalf("第 %d 次摘要漂移:\n %q\n %q", i, got, want)
		}
	}
	if b, err := json.Marshal(LimitBrief(codes, rows)); err != nil || !bytes.Contains(b, []byte("涨跌停影子")) {
		t.Errorf("摘要序列化异常: %s %v", b, err)
	}
}
