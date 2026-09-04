package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
)

// Tool 单个 MCP 工具定义与处理函数。
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	Handler     func(ctx context.Context, args map[string]interface{}) (interface{}, error)
}

// defaultActor 未显式提供 actor 时的操作者标识。
const defaultActor = "mcp-agent"

// dateProp / actorProp 跨工具复用的参数描述。
var (
	dateProp  = strProp("交易日 YYYYMMDD，缺省为今天（Asia/Shanghai）")
	actorProp = strProp("操作者，缺省 mcp-agent")
)

// errBadTicketStatus 指令单状态名不在状态机内。
var errBadTicketStatus = errors.New("非法指令单状态（取值 drafted/issued/filled/skipped/expired）")

// registerTools 注册全部工具：读工具见 tools_read.go，写工具见 tools_write.go。
func (s *Server) registerTools() {
	s.registerReadTools()
	s.registerWriteTools()
}

func objSchema(props map[string]interface{}, required []string) map[string]interface{} {
	return map[string]interface{}{"type": "object", "properties": props, "required": required}
}

// checkRequiredArgs 服务端强制校验工具自己声明的必填参数（含数组项内的必填字段）。
//
// `required` 写在 schema 里却不在服务端检查，等于对外承诺了一个不成立的契约：
// 外部 agent 少传 `positions`、或把 `reason` 传成空串，就会落进 handler 的"缺省值"分支，
// 把一次错误调用变成一次静默写入（sync_portfolio 会在不动持仓的情况下重写现金锚点）。
func checkRequiredArgs(t *Tool, args map[string]interface{}) error {
	req, _ := t.InputSchema["required"].([]string)
	for _, name := range req {
		if v, ok := args[name]; !ok || isEmptyArg(v) {
			return fmt.Errorf("缺少必填参数 %s（工具 %s）", name, t.Name)
		}
	}
	return checkArrayItems(t, args)
}

// checkArrayItems 校验数组参数里每一项自己声明的 required 字段。
//
// 顶层校验管不到嵌套：`positions` 给了非空数组就算通过，项内少一个 `total_qty`
// 会落进 argInt 的缺省 0，于是"这只票清仓了"被当成一次合法校准写进账本，
// 而响应依然是成功。这里挡的是**键不存在**，不是值为 0 ——
// 显式传 total_qty:0 仍是合法校准（券商那边确实卖光了），数字 0 不算空。
func checkArrayItems(t *Tool, args map[string]interface{}) error {
	props, _ := t.InputSchema["properties"].(map[string]interface{})
	for name, spec := range props {
		m, _ := spec.(map[string]interface{})
		if m["type"] != "array" {
			continue
		}
		items, _ := m["items"].(map[string]interface{})
		itemReq, _ := items["required"].([]string)
		if len(itemReq) == 0 {
			continue
		}
		list, _ := args[name].([]interface{})
		for i, e := range list {
			row, ok := e.(map[string]interface{})
			if !ok {
				return fmt.Errorf("参数 %s 第 %d 项必须是对象（工具 %s）", name, i+1, t.Name)
			}
			for _, k := range itemReq {
				if v, has := row[k]; !has || isEmptyArg(v) {
					return fmt.Errorf("参数 %s 第 %d 项缺少必填字段 %s（工具 %s）", name, i+1, k, t.Name)
				}
			}
		}
	}
	return nil
}

// dateArgKeys 携带交易日的参数名：12 个工具共用同一格式约定（YYYYMMDD）。
var dateArgKeys = []string{"date", "until"}

// checkDateArgs 分发前统一校验日期参数。
//
// 放在这里而不是各 handler 里，是因为下游 market.QuarterOf / PrevTradeDay 按 date[:4]
// 定长切片：一个短串会在任务里 panic，被调度器 recover 成一条说不清原因的失败。
// 空串放过＝"用当天"，由各 handler 自行取 today。
func checkDateArgs(t *Tool, args map[string]interface{}) error {
	for _, k := range dateArgKeys {
		v, ok := args[k]
		if !ok {
			continue
		}
		s, isStr := v.(string)
		if !isStr {
			return fmt.Errorf("工具 %s 的参数 %s 必须是日期字符串，收到 %T", t.Name, k, v)
		}
		if s == "" {
			continue
		}
		if err := market.CheckDate(s); err != nil {
			return fmt.Errorf("工具 %s 的参数 %s: %w", t.Name, k, err)
		}
	}
	return nil
}

// isEmptyArg 空串与空数组按"没给"处理：键在不在不重要，给的是不是内容才重要。
// 数字 0 不在此列（它是否合法由各工具的业务校验判断，如金额必须 >0）。
func isEmptyArg(v interface{}) bool {
	switch x := v.(type) {
	case string:
		return x == ""
	case []interface{}:
		return len(x) == 0
	default:
		return false
	}
}

// nonNil 空结果输出为 []。Go 的 nil 切片会序列化成 null，
// 外部 agent 按数组遍历时会直接踩空——"今日无数据"必须是空数组而不是 null。
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}
func numProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "number", "description": desc}
}

// ---- 参数解析辅助 ----

// today 当日交易日串。用交易所时区而非机器时区：
// 常驻进程跑在 UTC 主机上时，北京时间 08:00 前机器日期还是昨天，
// agent 不带 date 调用就会读到前一天的状态。
func today() string { return time.Now().In(market.Loc).Format("20060102") }

func argStr(a map[string]interface{}, k, def string) string {
	if v, ok := a[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return def
}

func argInt(a map[string]interface{}, k string, def int) int {
	if v, ok := a[k]; ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			if i, err := fmt.Sscanf(n, "%d", new(int)); err == nil {
				return i
			}
		}
	}
	return def
}

func argInt64(a map[string]interface{}, k string, def int64) int64 {
	if v, ok := a[k]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		}
	}
	return def
}

func argFloat(a map[string]interface{}, k string, def float64) float64 {
	if v, ok := a[k]; ok {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return def
}

// argArr 读取对象数组参数（sync_portfolio 的 positions）。
func argArr(a map[string]interface{}, k string) []map[string]interface{} {
	v, ok := a[k]
	if !ok {
		return nil
	}
	list, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(list))
	for _, e := range list {
		if m, ok := e.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}
