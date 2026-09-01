package tushare

import "testing"

// 用例取自本账号实测返回的原文, 以及 Tushare 限流报文的既有措辞
func TestClassify(t *testing.T) {
	cases := []struct {
		name            string
		code            int
		msg             string
		wantPermanent   bool
		wantRateLimited bool
	}{
		{"无接口权限", 40203, "抱歉，您没有接口(research_report)访问权限，权限的具体详情访问：https://tushare.pro/document/1?doc_id=108。", true, false},
		{"档位不足", 40209, "抱歉，您没有权限访问该接口", true, false},
		{"token无效", 40101, "您的token不对，请确认。", true, false},
		{"接口名错误", 40101, "请指定正确的接口名", true, false},
		{"缺必填参数", 50101, "必填参数, ts_code", true, false},
		{"日配额用尽", 40103, "抱歉，您每天最多访问该接口10次", true, false},
		{"分钟限流", 40103, "抱歉，您每分钟最多访问该接口125次", false, true},
		{"未知错误可重试", 9999, "服务器拥挤，请稍后再试", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify("some_api", tc.code, tc.msg)
			if got.Permanent != tc.wantPermanent {
				t.Errorf("Permanent = %v, 期望 %v (reason=%s)", got.Permanent, tc.wantPermanent, got.Reason)
			}
			if got.RateLimited != tc.wantRateLimited {
				t.Errorf("RateLimited = %v, 期望 %v (reason=%s)", got.RateLimited, tc.wantRateLimited, got.Reason)
			}
		})
	}
}

// 分钟限流与永久错误必须互斥: 同时置位会让 call() 先按永久错误返回, 丢掉唯一一次重试机会
func TestClassifyNeverBoth(t *testing.T) {
	for _, msg := range []string{
		"抱歉，您每分钟最多访问该接口125次",
		"抱歉，您每天最多访问该接口10次",
		"您的token不对，请确认。",
		"必填参数, ts_code",
	} {
		got := classify("x", 40203, msg)
		if got.Permanent && got.RateLimited {
			t.Errorf("msg=%q 同时判定为永久与限流", msg)
		}
	}
}
