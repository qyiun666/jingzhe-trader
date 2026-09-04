// Package quote 实时行情源（L2 适配层）：只有 gotdx 一个源。
//
// 金额统一使用 model.Fen；外部 IO 仅在本包（§1.2）。
// 设计要点（docs/tech-constraints.md §6）：
//   - gotdx 是唯一实时源，默认节点不可达时探测切换最快可达节点（同一个源的换节点，不是换源）；
//   - 无备用源、无缓存兜底：拿不到当前价就整体失败，由调用方记失败并告警，
//     盘中用旧价判断止损等于用昨天的价格做今天的决定。
package quote

import "jingzhe-trader/internal/model"

// Quote 单标的实时报价（金额统一为 model.Fen）。
type Quote struct {
	TsCode     string
	Price      model.Fen // 最新价
	PreClose   model.Fen
	Open       model.Fen
	High       model.Fen
	Low        model.Fen
	ServerTime string
	Source     string // gotdx
}

// Fetch 语义（GotdxSource.Fetch 即唯一实现）：要么每个请求标的都拿到正价，
// 要么返回 error —— 不存在"部分成功"，也没有备用源和缓存旧价。
