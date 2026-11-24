package scheduler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestScreenSnapshotCallSiteDerivesLimitBrief 接线锁（T13，与接线同 commit）：调用
// writeScreenSnapshot 的那个函数体内必须真的存在一次 LimitBrief 取用，并且**最后一个实参**
// 就是它（或由它赋值的局部变量）。
//
// 为什么要有这一条：涨跌停观测没有开关（组长 11-18 20:25 裁定②），「limit_brief 键没出现」
// 只剩一个解释——写那行 detail 的二进制还没接线。signal 侧的产出用例与 model 侧的载荷形状用例
// 都只能证明"函数对"、证不了"当日那行里真有这个键"；11-10 的故障形态正是产出齐备、消费零调用，
// 而彼时全仓测试全绿。接线锁必须与接线同 commit，否则锁不住。
//
// 变异对照（本批落地后逐条实测，任何一条没红就是这条锁失效）：
//
//	① 调用点最后实参换成 ""                      → 必须红（omitempty 会让键整个消失）
//	② 调用点最后实参换成 rep.SectorShadowBrief() → 必须红（两个观测键同形，归因作废）
//	③ LimitBrief 取了但结果只落 _ / 没进实参      → 必须红（本条比 sector 先例多出来的强度）
//	④ 接线整段删掉                                → 必须红（checked==0 兜住改名或搬走）
func TestScreenSnapshotCallSiteDerivesLimitBrief(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "pipeline.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 pipeline.go 失败（接线锁不能空跑）: %v", err)
	}
	checked := 0
	ast.Inspect(file, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			return true
		}
		var (
			callsWriter, briefTaken, briefIsLastArg bool
			argCount                                int
			briefVars                               = map[string]bool{}
		)
		ast.Inspect(fd.Body, func(m ast.Node) bool {
			switch e := m.(type) {
			case *ast.CallExpr:
				if id, ok := e.Fun.(*ast.Ident); ok && id.Name == "writeScreenSnapshot" {
					callsWriter = true
					argCount = len(e.Args)
					if len(e.Args) > 0 {
						briefIsLastArg = isLimitBriefArg(e.Args[len(e.Args)-1], briefVars)
					}
				} else if sel, ok := e.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "LimitBrief" {
					briefTaken = true
				}
			case *ast.AssignStmt:
				// 允许「先落局部变量再传参」：只认右侧确实是 LimitBrief(...) 调用的那一种赋值。
				call, ok := rhsCall(e.Rhs)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "LimitBrief" {
					return true
				}
				briefTaken = true
				for _, l := range e.Lhs {
					if id, ok := l.(*ast.Ident); ok {
						briefVars[id.Name] = true
					}
				}
			}
			return true
		})
		if !callsWriter {
			return true
		}
		checked++
		if !briefTaken {
			t.Errorf("%s 调用了 writeScreenSnapshot 却没取用 LimitBrief ⇒ 当日 detail 里不会出现 limit_brief 键", fd.Name.Name)
		}
		if !briefIsLastArg {
			t.Errorf("%s 传给 writeScreenSnapshot 的第 %d 个（最后一个）实参不是 LimitBrief 的结果 ⇒ 接线接错了位置或被写成常量",
				fd.Name.Name, argCount)
		}
		return true
	})
	if checked == 0 {
		t.Fatal("pipeline.go 里找不到 writeScreenSnapshot 调用点：接线锁本身失效了（改名/搬走也要有人发现）")
	}
}

// isLimitBriefArg 末位实参是否就是 limit brief：直接调用，或名字来自上面登记过的局部变量。
func isLimitBriefArg(e ast.Expr, briefVars map[string]bool) bool {
	switch v := e.(type) {
	case *ast.CallExpr:
		sel, ok := v.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "LimitBrief"
	case *ast.Ident:
		return briefVars[v.Name]
	}
	return false
}

// rhsCall 取赋值语句右侧唯一的那个调用表达式；右侧不是单个调用（含元组赋值）时返回 false。
func rhsCall(rhs []ast.Expr) (*ast.CallExpr, bool) {
	if len(rhs) != 1 {
		return nil, false
	}
	call, ok := rhs[0].(*ast.CallExpr)
	return call, ok
}
