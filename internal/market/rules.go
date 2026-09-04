package market

import (
	"strings"

	"jingzhe-trader/internal/model"
)

// ===================== 停牌 / 整手 / T+1 规则 =====================

// IsSTName 是否 ST / *ST / 退市整理：只看名称前缀。
//
// 不落 is_st 列的原因是名称就是唯一真相源 —— 戴帽摘帽先反映在名称上，Tushare 的
// stock_basic 接口又带 name 字段，另存一个布尔只会出现"名称已摘帽、标记还是 ST"。
func IsSTName(name string) bool {
	n := strings.ToUpper(strings.Join(strings.Fields(name), "")) // 去掉全部空白
	n = strings.TrimPrefix(n, "*")                               // *ST
	for strings.HasPrefix(n, "S") && !strings.HasPrefix(n, "ST") {
		n = strings.TrimPrefix(strings.TrimPrefix(n, "S"), "*") // SST / S*ST
	}
	return strings.HasPrefix(n, "ST") || strings.Contains(n, "退")
}

// RoundLotDown 向下取整到 100 股。
func RoundLotDown(q model.Qty) model.Qty {
	return q.RoundLotDown()
}

// RoundLotUp 向上取整到 100 股。
func RoundLotUp(q model.Qty) model.Qty {
	return q.RoundLotUp()
}
