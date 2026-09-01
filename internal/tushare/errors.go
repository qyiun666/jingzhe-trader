package tushare

import (
	"fmt"
	"strings"
)

// APIError Tushare 业务错误 (HTTP 200 但响应 code != 0)
type APIError struct {
	API         string
	Code        int
	Msg         string
	Reason      string // 判定依据, 便于日志定位为什么没重试
	Permanent   bool   // 重试不会改变结果 (无权限/token 错/参数非法/日配额用尽)
	RateLimited bool   // 分钟级限流: 退避到下一个时间窗后重试有意义
}

func (e *APIError) Error() string {
	return fmt.Sprintf("tushare API %s 返回错误: code=%d msg=%s", e.API, e.Code, e.Msg)
}

// classify 判定业务错误可否重试
//
// Tushare 的错误码粒度很粗 (40101 同时覆盖"token 不对"与"接口名错误"), 只能以报文关键词为主、
// 错误码为辅。判错的代价不对称: 把永久错误当可重试, 一次调用白烧 4 个请求和 16s 退避等待
// (实测 token 配置错误就是这样把整轮同步拖长), 在按分钟/按天计次的档位上还会顺带挤掉当天正常配额。
func classify(api string, code int, msg string) *APIError {
	err := &APIError{API: api, Code: code, Msg: msg}
	switch {
	case containsAny(msg, "每天", "每日", "当天", "每月"):
		// 日配额已用尽: 当天再重试只会重复失败。必须排在分钟判断之前——
		// Tushare 的日配额与分钟限流都写作"最多访问…次", 只靠该词区分不出窗口
		err.Permanent = true
		err.Reason = "日配额已用尽"
	case containsAny(msg, "每分钟", "每小时") || strings.Contains(msg, "最多访问"):
		err.RateLimited = true
		err.Reason = "分钟级限流"
	case code == 40203 || code == 40209 || strings.Contains(msg, "没有接口"):
		err.Permanent = true
		err.Reason = "当前档位无该接口权限"
	case strings.Contains(msg, "token"):
		err.Permanent = true
		err.Reason = "token 无效"
	case code == 50101 || containsAny(msg, "必填参数", "参数错误", "参数不合法") || strings.Contains(msg, "接口名"):
		err.Permanent = true
		err.Reason = "请求参数或接口名非法"
	default:
		err.Reason = "未知错误码, 按可重试处理"
	}
	return err
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
