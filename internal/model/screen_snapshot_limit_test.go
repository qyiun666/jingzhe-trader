package model

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// T10 形态锁 + T12′（载荷面）：limit_brief 只能追加在结构体末尾、且必须带 omitempty。
//
// 为什么要钉住"末尾"：run_trace.detail 存的就是这份 JSON 的逐字节文本，归因与逐日 diff 都在比
// 这句；新键插到中间会让同一天的历史载荷与新代码写出的载荷形状不一致（前两个观测字段的注释
// 写的就是这条），所以锁声明序而不是锁文档。
//
// 为什么要钉住 omitempty：涨跌停观测是无条件的（11-18 20:25 裁定②），算过就必出句子，
// 于是键缺失只剩一个解释——写这行的二进制还没接线。反过来，若哪天有人去掉 omitempty，
// 接线前的旧数据回放会凭空多出一个空串键，"缺键=旧二进制"这条推断当场失效。
func TestScreenSnapshotLimitBriefShapeLock(t *testing.T) {
	want := []string{"trade_date", "total_asset_fen", "slot_fen", "candidates", "sector_shadow_brief", "limit_brief"}
	typ := reflect.TypeOf(ScreenSnapshot{})
	if typ.NumField() != len(want) {
		t.Fatalf("ScreenSnapshot 字段数变了（%d 个，期望 %d 个）：新增观测字段一律追加末尾",
			typ.NumField(), len(want))
	}
	for i, w := range want {
		tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if tag != w {
			t.Errorf("第 %d 个字段键名 = %q，期望 %q（键序跟随声明序，插中间会让历史载荷逐字节漂移）", i, tag, w)
		}
	}
	if f, _ := typ.FieldByName("LimitBrief"); !strings.Contains(f.Tag.Get("json"), "omitempty") {
		t.Error("limit_brief 必须带 omitempty：空串写出键 ⇒ 「缺键只可能来自旧二进制」失效")
	}

	var snap ScreenSnapshot
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "limit_brief") {
		t.Fatalf("未赋值时不得写出该键：%s", b)
	}
	snap.LimitBrief = "涨跌停影子 0/候选 0 只（…）：当日候选无一触板，判定已生效。"
	b, err = json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "判定已生效。\"}") {
		t.Errorf("该键必须落在载荷末尾：%s", b)
	}
}
