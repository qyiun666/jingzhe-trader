package model

// Stock 股票基本信息
type Stock struct {
	TsCode     string `json:"ts_code" db:"ts_code"`         // 代码 000001.SZ
	Symbol     string `json:"symbol" db:"symbol"`           // 代码 000001
	Name       string `json:"name" db:"name"`               // 名称
	Market     string `json:"market" db:"market"`           // 市场 主板/创业板/科创板/北交所
	Exchange   string `json:"exchange" db:"exchange"`       // 交易所 SSE/SZSE/BSE
	IsST       bool   `json:"is_st" db:"is_st"`             // 是否ST
	ListStatus string `json:"list_status" db:"list_status"` // L上市 D退市 P暂停
	ListDate   string `json:"list_date" db:"list_date"`     // 上市日期 YYYYMMDD
	DelistDate string `json:"delist_date" db:"delist_date"` // 退市日期
}

// Board 板块类型
type Board int

const (
	BoardUnknown Board = iota
	BoardMainSH        // 沪市主板
	BoardMainSZ        // 深市主板
	BoardChiNext       // 创业板 300
	BoardSTAR          // 科创板 688
	BoardBSE           // 北交所 8/4
)

// DetectBoard 根据代码识别板块
func DetectBoard(tsCode string) Board {
	if len(tsCode) < 6 {
		return BoardUnknown
	}
	symbol := tsCode[:6]
	switch {
	case len(symbol) >= 3 && symbol[:3] == "688":
		return BoardSTAR
	case len(symbol) >= 3 && symbol[:3] == "300":
		return BoardChiNext
	case len(symbol) >= 1 && (symbol[0] == '6'):
		return BoardMainSH
	case len(symbol) >= 1 && (symbol[0] == '0' || symbol[0] == '3'):
		return BoardMainSZ
	case len(symbol) >= 1 && (symbol[0] == '8' || symbol[0] == '4'):
		return BoardBSE
	default:
		return BoardUnknown
	}
}

// MarketFromCode 从代码推断市场描述
func MarketFromCode(tsCode string) string {
	switch DetectBoard(tsCode) {
	case BoardSTAR:
		return "科创板"
	case BoardChiNext:
		return "创业板"
	case BoardMainSH:
		return "沪市主板"
	case BoardMainSZ:
		return "深市主板"
	case BoardBSE:
		return "北交所"
	default:
		return "未知"
	}
}
