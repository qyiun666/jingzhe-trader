// Package tushare Tushare HTTP 适配层（L2）。
//
// 职责：限流 + 错误分类 + 指数退避重试 + 将返回值解码为 model 类型。
// 外部 IO 只在适配层；store/业务层禁止 net/http（ARCHITECTURE §1.2）。
// money/price 一律 model.Fen；float→Fen 边界集中在 decode.go（§11.4）。
package tushare

import (
	"errors"
	"fmt"
)

// Kind 错误分类（ARCHITECTURE §11.1）。
type Kind int

const (
	// KindTransient 瞬时错误：指数退避重试。
	KindTransient Kind = iota
	// KindRateLimited 频率限制：整窗等待后重试。
	KindRateLimited
	// KindPermanent 永久错误：不重试，直接落告警。
	KindPermanent
)

func (k Kind) String() string {
	switch k {
	case KindTransient:
		return "transient"
	case KindRateLimited:
		return "rate_limited"
	case KindPermanent:
		return "permanent"
	default:
		return "unknown"
	}
}

// APIError Tushare 接口返回的结构化错误。
type APIError struct {
	API  string // 接口名（用于告警定位）
	Code int    // Tushare 错误码
	Msg  string // 错误描述
	Kind Kind   // 分类
	Err  error  // 底层错误（网络/JSON 等）
}

func (e *APIError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("tushare %s 失败 code=%d(%s) msg=%q: %v", e.API, e.Code, e.Kind, e.Msg, e.Err)
	}
	return fmt.Sprintf("tushare %s 失败 code=%d(%s) msg=%q", e.API, e.Code, e.Kind, e.Msg)
}

func (e *APIError) Unwrap() error { return e.Err }

// 哨兵错误（跨包判等用 errors.Is）。
var (
	// ErrPermanentAPI 永久错误（无权限/接口名错/积分不足），不重试。
	ErrPermanentAPI = errors.New("tushare permanent api error")
	// ErrRateLimited 频率限制，整窗等待后重试。
	ErrRateLimited = errors.New("tushare rate limited")
	// ErrTransient 瞬时错误，指数退避重试。
	ErrTransient = errors.New("tushare transient error")
)

// Classify 根据 Tushare 错误码分类（实测结论见 docs/tech-constraints.md §2.3）。
//
//	永久（不重试）：40101 接口名错 / 40203 无权限 / 403xx 权限积分 / 426xx 无权限申请
//	频率（整窗等待）：42901 每分钟访问次数超限
//	其余（50000 系统异常、网络错等）归为瞬时（指数退避）
func Classify(code int) Kind {
	switch code {
	case 40101, 40203, 40301, 40302, 40303, 42601, 42602, 42603:
		return KindPermanent
	case 42901:
		return KindRateLimited
	default:
		return KindTransient
	}
}
