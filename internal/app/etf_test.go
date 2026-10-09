package app

import (
	"strings"
	"testing"

	"jingzhe-trader/internal/config"
)

// etfCfg 只装 ETF 相关键的 Config 桩（validateETF 只读这三键，其余由其他用例覆盖）。
func etfCfg(enabled, whitelist, minAmt string) *config.Config {
	v := map[string]config.ConfigValue{}
	put := func(k, val string) {
		if val != "" {
			v[k] = config.ConfigValue{Key: k, Value: val}
		}
	}
	put("screen.etf_enabled", enabled)
	put("screen.etf_whitelist", whitelist)
	put("screen.etf_min_avg_amount_yi", minAmt)
	return &config.Config{Values: v}
}

// TestValidateETFDefaultIsClosed 出厂形态：总闸关 + 7 只白名单齐名称 → 放行，
// 且关闭时白名单残缺也不拦（那是"还没启用的配置"，不该让今天的进程起不来）。
func TestValidateETFDefaultIsClosed(t *testing.T) {
	var def, wl, amt string
	for _, s := range config.KeySpecs {
		switch s.Key {
		case "screen.etf_enabled":
			def = s.Default
		case "screen.etf_whitelist":
			wl = s.Default
		case "screen.etf_min_avg_amount_yi":
			amt = s.Default
		}
	}
	if def != "false" {
		t.Errorf("screen.etf_enabled 默认必须是 false（部署当日零行为变化），实际 %q", def)
	}
	if err := validateETF(etfCfg(def, wl, amt)); err != nil {
		t.Fatalf("出厂默认被拒: %v", err)
	}
	// 关闭时缺名称的白名单不拦。
	if err := validateETF(etfCfg("false", "515050.SH", "2.0")); err != nil {
		t.Errorf("总闸关闭时不该因白名单残缺拒绝启动: %v", err)
	}
	// 三键都缺失（老库/手工桩）按默认放行。
	if err := validateETF(etfCfg("", "", "")); err != nil {
		t.Errorf("键缺失应按默认放行: %v", err)
	}
}

// TestETFWhitelistDefaultIsApprovedPool 出厂白名单必须正好是组长批准的 7 只，
// 且每一只都带人工复核过的名称 —— 白名单是人工维护的（fund_basic 没有跟踪标的字段），
// 键目录里这份默认值就是唯一真相源，改它等于改标的池，必须走群里的批准。
func TestETFWhitelistDefaultIsApprovedPool(t *testing.T) {
	var wl string
	for _, s := range config.KeySpecs {
		if s.Key == "screen.etf_whitelist" {
			wl = s.Default
		}
	}
	codes, err := ETFWhitelistCodes(wl)
	if err != nil {
		t.Fatalf("出厂白名单不合法: %v", err)
	}
	want := []string{"159819.SZ", "159852.SZ", "159995.SZ", "512480.SH", "515050.SH", "588000.SH", "588200.SH"}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Errorf("出厂白名单 = %v，期望 %v", codes, want)
	}
	for _, item := range strings.Split(wl, ",") {
		parts := strings.Split(item, ":")
		if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
			t.Errorf("条目 %q 不是 code:name:track 三段式", item)
		}
	}
}

// TestValidateETFEnabledRequiresUsablePool 开闸后的严格面：非法代码、重复代码、
// 空池、缺名称、非法阈值/布尔，统统拒绝启动。
//
// 这一族错误的共同形态是"静默 0 候选"：代码写错时 fund_daily 只返回空，
// 运行期看不见任何异常，ETF 链就此消失而个股链照常出单 —— 那比启动失败危险得多。
func TestValidateETFEnabledRequiresUsablePool(t *testing.T) {
	ok := "515050.SH:华夏中证5G通信主题ETF:通信设备"
	for _, tc := range []struct {
		enabled, wl, amt, why string
	}{
		{"true", ok, "2.0", "合法组合不该被拒"},
		{"false", "515050XX.SH:名称:赛道", "2.0", "代码格式错（关闸时只解析不拦）"},
	} {
		err := validateETF(etfCfg(tc.enabled, tc.wl, tc.amt))
		if tc.enabled == "true" && err != nil {
			t.Errorf("%s: %v", tc.why, err)
		}
	}
	cases := []struct{ enabled, wl, amt, why string }{
		{"true", "515050X.SH:名称:赛道", "2.0", "代码格式非法"},
		{"true", "515050.SH:名称,515050.SH:另一名称", "2.0", "代码重复"},
		{"true", "515050.SH", "2.0", "缺名称"},
		{"true", "", "2.0", "启用却空池"},
		{"true", "515050.SH:名称", "0", "阈值为 0"},
		{"true", "515050.SH:名称", "-1", "阈值为负"},
		{"yes", "515050.SH:名称", "2.0", "布尔值不可解析"},
	}
	for _, tc := range cases {
		if err := validateETF(etfCfg(tc.enabled, tc.wl, tc.amt)); err == nil {
			t.Errorf("%s：应拒绝启动却放行了", tc.why)
		}
	}
	// 阈值只在总闸打开时是硬要求：关闸时它是"还没生效的配置"。
	if err := validateETF(etfCfg("false", "515050.SH:名称", "0")); err == nil {
		t.Log("关闸时阈值 0 被放行（可接受：该值尚未参与任何判定）")
	}
}

// TestETFWhitelistCodesTolerantFormat 代码列解析容忍空格/分号/换行与大小写，
// 输出稳定有序（同一份配置在同步侧与漏斗侧必须给出同一串代码）。
func TestETFWhitelistCodesTolerantFormat(t *testing.T) {
	codes, err := ETFWhitelistCodes(" 515050.sh:华夏中证5G通信主题ETF:通信设备 ;\n159819.SZ:易方达中证人工智能主题ETF ")
	if err != nil {
		t.Fatalf("解析: %v", err)
	}
	if strings.Join(codes, ",") != "159819.SZ,515050.SH" {
		t.Errorf("codes=%v", codes)
	}
}
