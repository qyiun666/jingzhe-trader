package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestDefaultDocumentCoversEveryField 结构体里每个可配置字段都必须有代码默认值。
//
// 背景: 配置源从 YAML 换成 SQLite 后, 首次启动靠 DefaultJSON() 种子。此前 risk.* / strategy.* /
// goal.* 等 49 个键只存在于配置文件、没有代码默认值, 空库种子出来全是 Go 零值 ——
// 而 risk.max_position_pct=0 会让风控把每笔买入裁成 0 股, 属于静默的危险配置。
// 新增字段却忘了 SetDefault 会直接命中这个用例。
func TestDefaultDocumentCoversEveryField(t *testing.T) {
	doc, err := DefaultJSON()
	if err != nil {
		t.Fatalf("DefaultJSON 失败: %v", err)
	}
	var flat map[string]any
	if err := json.Unmarshal(doc, &flat); err != nil {
		t.Fatalf("默认文档不是合法 JSON: %v", err)
	}
	present := map[string]bool{}
	flatten("", flat, present)

	// 凭据不得有默认值: 源码里不能出现任何密钥字面量
	noDefault := map[string]bool{
		"tushare.token": true,
		"llm.api_key":   true,
		"mail.password": true,
	}

	var missing []string
	for _, path := range structLeafPaths(reflect.TypeOf(Config{}), "") {
		if noDefault[path] {
			if present[path] {
				t.Errorf("凭据键 %s 不应有代码默认值", path)
			}
			continue
		}
		if !present[path] {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d 个配置字段缺少代码默认值, 空库种子后会落到 Go 零值: %s",
			len(missing), strings.Join(missing, ", "))
	}
}

// TestCriticalDefaultsAreUsable 关键风控与策略默认值必须是可用值而非零值
func TestCriticalDefaultsAreUsable(t *testing.T) {
	doc, err := DefaultJSON()
	if err != nil {
		t.Fatalf("DefaultJSON: %v", err)
	}
	cfg, err := LoadFromJSON(doc)
	if err != nil {
		t.Fatalf("LoadFromJSON(默认文档) 失败: %v", err)
	}
	if cfg.Risk.MaxPositionPct <= 0 || cfg.Risk.MaxPositionPct > 1 {
		t.Errorf("risk.max_position_pct 默认值不可用: %v", cfg.Risk.MaxPositionPct)
	}
	if cfg.Risk.StopLossPct <= 0 {
		t.Errorf("risk.stop_loss_pct 默认值为 0, 止损会失效")
	}
	if cfg.Strategy.MACross.ShortPeriod <= 0 || cfg.Strategy.MACross.LongPeriod <= cfg.Strategy.MACross.ShortPeriod {
		t.Errorf("ma_cross 均线周期默认值非法: short=%d long=%d",
			cfg.Strategy.MACross.ShortPeriod, cfg.Strategy.MACross.LongPeriod)
	}
	if cfg.Broker.Type == "" {
		t.Errorf("broker.type 默认值为空, 券商分支会走错")
	}
}

// TestLoadFromJSONEmptyDocFallsBackToDefaults 空文档(全新库尚未种子)必须回落默认值而非报错
func TestLoadFromJSONEmptyDocFallsBackToDefaults(t *testing.T) {
	cfg, err := LoadFromJSON(nil)
	if err != nil {
		t.Fatalf("空文档不应报错: %v", err)
	}
	if cfg.Tushare.BaseURL != "http://api.tushare.pro" {
		t.Errorf("应拿到默认值, 实际 %q", cfg.Tushare.BaseURL)
	}
	if _, err := EffectiveJSON(nil); err != nil {
		t.Errorf("空文档 dump 不应报错: %v", err)
	}
}

func flatten(prefix string, m map[string]any, out map[string]bool) {
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if child, ok := v.(map[string]any); ok {
			flatten(path, child, out)
			continue
		}
		out[path] = true
	}
}

// structLeafPaths 按 mapstructure tag 递归收集 Config 的全部叶子路径
func structLeafPaths(t reflect.Type, prefix string) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := f.Tag.Get("mapstructure")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out = append(out, structLeafPaths(ft, path)...)
			continue
		}
		out = append(out, path)
	}
	return out
}
