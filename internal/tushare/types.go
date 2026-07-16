package tushare

import (
	"fmt"
	"strconv"
)

// Request Tushare API 请求体
// API 通过 HTTP POST 提交, body 为该结构的 JSON 编码
type Request struct {
	APIName string                 `json:"api_name"` // 接口名称, 例如 stock_basic / daily
	Token   string                 `json:"token"`    // 用户 token
	Params  map[string]interface{} `json:"params"`   // 接口参数
	Fields  string                 `json:"fields"`   // 需要返回的字段, 逗号分隔; 为空则返回全部
}

// ResponseData 响应中的数据部分
// Tushare 返回的数据为列存: fields 为列名, items 中每行为按 fields 顺序排列的值
type ResponseData struct {
	Fields []string        `json:"fields"`
	Items  [][]interface{} `json:"items"`
}

// Response Tushare API 响应体
type Response struct {
	Code int          `json:"code"` // 0 表示成功, 其它为错误码
	Msg  string       `json:"msg"`  // 错误信息
	Data ResponseData `json:"data"` // 数据
}

// AdjFactor 复权因子记录
type AdjFactor struct {
	TsCode    string  `json:"ts_code"`
	TradeDate string  `json:"trade_date"`
	AdjFactor float64 `json:"adj_factor"`
}

// parseFloat 兼容解析 Tushare 返回的各种数值类型为 float64
// 支持 float64/float32/int/int64/string, 空值(nil 或空字符串)返回 0
func parseFloat(v interface{}) float64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int32:
		return float64(val)
	case int64:
		return float64(val)
	case string:
		if val == "" {
			return 0
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0
		}
		return f
	case bool:
		return 0
	default:
		// 兜底: 尝试通过 fmt 解析数字字符串
		f, err := strconv.ParseFloat(fmt.Sprintf("%v", v), 64)
		if err != nil {
			return 0
		}
		return f
	}
}

// parseInt 兼容解析 Tushare 返回的各种数值类型为 int
func parseInt(v interface{}) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return int(val)
	case float32:
		return int(val)
	case int:
		return val
	case int32:
		return int(val)
	case int64:
		return int(val)
	case string:
		if val == "" {
			return 0
		}
		i, err := strconv.Atoi(val)
		if err == nil {
			return i
		}
		// 字符串可能带小数, 退化为 float 解析
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0
		}
		return int(f)
	case bool:
		if val {
			return 1
		}
		return 0
	default:
		f, err := strconv.ParseFloat(fmt.Sprintf("%v", v), 64)
		if err != nil {
			return 0
		}
		return int(f)
	}
}

// parseString 兼容解析 Tushare 返回的各种类型为 string
// 数值类型会被格式化为字符串(整数无小数点)
func parseString(v interface{}) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		// 整数值去掉小数点, 例如日期 20210101.0 -> "20210101"
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case float32:
		f := float64(val)
		if f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int32:
		return strconv.FormatInt(int64(val), 10)
	case int64:
		return strconv.FormatInt(val, 10)
	case bool:
		if val {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", v)
	}
}
