package tushare

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"jingzhe-trader/internal/model"
)

// fenType 用于区分 model.Fen（需经 FromFloat 四舍五入转换）与普通 int64 字段。
var fenType = reflect.TypeOf(model.Fen(0))

// DecodeItems 将 Tushare 返回的 fields/items 解码为目标切片（dst 为 *[]Struct）。
//
// 字段匹配：按结构体 db tag（与 Tushare 返回字段名一致）映射；
// 金额类 model.Fen 由 float64 经 model.FromFloat 转为分（四舍五入）；
// 其余按 Go 类型转换。这是系统仅有的两处 float→Fen 边界之一（ARCHITECTURE §11.4）。
func DecodeItems(fields []string, items [][]interface{}, dst interface{}) error {
	dv := reflect.ValueOf(dst)
	if dv.Kind() != reflect.Ptr || dv.IsNil() {
		return fmt.Errorf("DecodeItems: dst 必须为非空指针")
	}
	slice := dv.Elem()
	if slice.Kind() != reflect.Slice {
		return fmt.Errorf("DecodeItems: dst 必须指向切片，实际 %s", slice.Kind())
	}
	elemType := slice.Type().Elem()
	if elemType.Kind() == reflect.Ptr {
		elemType = elemType.Elem()
	}

	// 建立 字段名(小写) -> 结构体字段下标（优先 db tag）
	fieldIdx := make(map[string]int, elemType.NumField())
	for i := 0; i < elemType.NumField(); i++ {
		f := elemType.Field(i)
		name := f.Name
		if tag := f.Tag.Get("db"); tag != "" {
			name = tag
		}
		fieldIdx[strings.ToLower(name)] = i
	}

	out := reflect.MakeSlice(slice.Type(), 0, len(items))
	for _, row := range items {
		elem := reflect.New(elemType).Elem()
		for ci, col := range row {
			if ci >= len(fields) {
				break
			}
			fi, ok := fieldIdx[strings.ToLower(fields[ci])]
			if !ok {
				continue
			}
			if err := setField(elem.Field(fi), col); err != nil {
				return fmt.Errorf("解码字段 %s 失败: %w", fields[ci], err)
			}
		}
		out = reflect.Append(out, elem)
	}
	slice.Set(out)
	return nil
}

// setField 将 Tushare 单元值（string / float64 / bool）写入结构体字段。
func setField(fv reflect.Value, val interface{}) error {
	if !fv.CanSet() {
		return nil
	}
	if val == nil {
		return nil
	}
	switch fv.Kind() {
	case reflect.String:
		s, _ := val.(string)
		fv.SetString(s)
	case reflect.Bool:
		switch v := val.(type) {
		case bool:
			fv.SetBool(v)
		case float64:
			fv.SetBool(v != 0)
		case string:
			fv.SetBool(v == "1" || v == "true" || v == "True")
		default:
			return fmt.Errorf("无法将 %T 转为 bool", val)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		switch v := val.(type) {
		case float64:
			fv.SetInt(int64(v))
		case int64:
			fv.SetInt(v)
		case int:
			fv.SetInt(int64(v))
		case string:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return err
			}
			fv.SetInt(n)
		default:
			return fmt.Errorf("无法将 %T 转为 int", val)
		}
	case reflect.Int64:
		switch v := val.(type) {
		case float64:
			if fv.Type() == fenType {
				// 金额：float → Fen（四舍五入），禁止截断（§11.4）
				fv.SetInt(int64(model.FromFloat(v)))
			} else {
				fv.SetInt(int64(v))
			}
		case int64:
			fv.SetInt(v)
		case int:
			fv.SetInt(int64(v))
		case string:
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return err
			}
			fv.SetInt(n)
		default:
			return fmt.Errorf("无法将 %T 转为 int64", val)
		}
	case reflect.Float64:
		switch v := val.(type) {
		case float64:
			fv.SetFloat(v)
		case int64:
			fv.SetFloat(float64(v))
		case string:
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return err
			}
			fv.SetFloat(n)
		default:
			return fmt.Errorf("无法将 %T 转为 float64", val)
		}
	default:
		return fmt.Errorf("不支持的字段类型 %s", fv.Kind())
	}
	return nil
}

// ===================== 原始 DTO 与 float→Fen 边界 =====================
// Tushare 返回字段名（vol/amount/total_mv/circ_mv）与 model 列名（vol_lot/amount_k/
// total_mv_w/circ_mv_w）不一致，故先解码到原始 DTO（float64 元/手/千元），再经下方
// 转换函数归一为 model（分/万元）。转换函数位于适配层，是本包两处 float→Fen 边界之一（§11.4）。

// RawBar 日线原始解码结构（Tushare 字段名，float64 元/手）。
// 只取实际入库的三列：close 是未复权收盘（转成 RawClose），vol 是成交量（手）。
type RawBar struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	Close     float64 `db:"close"`
	Vol       float64 `db:"vol"`
}

// ToModelBar 将原始日线转为 model.Bar（价格→分；Close 暂存未复权值，由同步侧就地复权）。
func ToModelBar(r RawBar) model.Bar {
	return model.Bar{
		TsCode:    r.TsCode,
		TradeDate: r.TradeDate,
		Close:     model.FromFloat(r.Close),
		VolLot:    r.Vol,
	}
}

// RawValuation 每日指标原始解码结构（Tushare 字段名）。
// circ_mv 单位为千元，ToModelValuation 换算为万元（模型口径）。
type RawValuation struct {
	TsCode       string  `db:"ts_code"`
	TurnoverRate float64 `db:"turnover_rate"`
	PETtm        float64 `db:"pe_ttm"`
	PB           float64 `db:"pb"`
	CircMv       float64 `db:"circ_mv"` // 千元
}

// ToModelValuation 将原始每日指标转为 model.Valuation（流通市值千元→万元）。
func ToModelValuation(r RawValuation) model.Valuation {
	return model.Valuation{
		TsCode:       r.TsCode,
		TurnoverRate: r.TurnoverRate,
		PETtm:        r.PETtm,
		PB:           r.PB,
		CircMvW:      r.CircMv / 10.0,
	}
}
