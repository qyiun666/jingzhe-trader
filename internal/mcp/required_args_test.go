package mcp

import (
	"strings"
	"testing"
)

// syncPortfolioTool 用生产里同一份 schema 构造，不复刻骨架：
// schema 改了而测试还在断言旧契约，等于给 #16 留了个假绿。
func syncPortfolioTool() *Tool {
	return &Tool{
		Name: "sync_portfolio",
		InputSchema: objSchema(map[string]interface{}{
			"date":                 dateProp,
			"initial_capital_yuan": numProp("本金（元）"),
			"available_cash_yuan":  numProp("券商口径的可用现金（元），必填且大于 0"),
			"positions":            positionsSchema(),
			"actor":                actorProp,
		}, []string{"positions", "available_cash_yuan"}),
	}
}

// TestRequiredArgsEmptyArrayIsAStateNotAMissing 是 #16 的契约。
//
// 11-03 16:01 两次 sync_portfolio {"positions":[]} 被打回"缺少必填参数 positions"：
// isEmptyArg 把空数组当空，于是"券商整本没有持仓"与"漏传 positions"共用一个错误。
// 修法只放开**显式空数组**这一格——键完全不传仍然必须拒，因为那条防的是
// 漏传退化成"只改现金锚点、持仓不动"的静默写入（原注释里的意图一个字不能丢）。
func TestRequiredArgsEmptyArrayIsAStateNotAMissing(t *testing.T) {
	tool := syncPortfolioTool()

	t.Run("空数组放行", func(t *testing.T) {
		args := map[string]interface{}{
			"positions":           []interface{}{},
			"available_cash_yuan": 11737.83,
		}
		if err := checkRequiredArgs(tool, args); err != nil {
			t.Fatalf("positions:[] 被拒了: %v（清仓后的校准没有正门可走，只能手改 config_kv 绕过量纲闸）", err)
		}
	})

	t.Run("键缺失仍拒", func(t *testing.T) {
		args := map[string]interface{}{"available_cash_yuan": 11737.83}
		err := checkRequiredArgs(tool, args)
		if err == nil {
			t.Fatal("不传 positions 竟然通过了：漏传会被当成只改锚点的静默写入")
		}
		if !strings.Contains(err.Error(), "positions") {
			t.Errorf("错误文案应点名 positions，实际 %v", err)
		}
	})

	t.Run("非数组必填的空值语义不变", func(t *testing.T) {
		args := map[string]interface{}{
			"positions":           []interface{}{},
			"available_cash_yuan": "", // 空串照旧算没给
		}
		if err := checkRequiredArgs(tool, args); err == nil {
			t.Fatal("available_cash_yuan 传空串竟然通过")
		}
	})

	t.Run("空数组里的项仍逐项校验", func(t *testing.T) {
		// 非空数组的项内校验不能因为顶层放开而退化：少 total_qty 必须还是错。
		args := map[string]interface{}{
			"positions": []interface{}{
				map[string]interface{}{"ts_code": "600704.SH"},
			},
			"available_cash_yuan": 11737.83,
		}
		err := checkRequiredArgs(tool, args)
		if err == nil || !strings.Contains(err.Error(), "total_qty") {
			t.Fatalf("项内必填校验退化了的信号: %v", err)
		}
	})
}
