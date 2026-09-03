// Package quote 实时行情源（L2 适配层）：gotdx 主源 + 腾讯降级备用源。
//
// 金额统一使用 model.Fen；外部 IO 仅在本包（§1.2）。
// 设计要点（docs/tech-constraints.md §6）：
//   - gotdx 为主实时源，主节点不可达时自动探测切换最快节点；
//   - 腾讯 qt.gtimg.cn 为降级备用源（GBK 解码）；
//   - 行情获取失败返回最近一次有效价（缓存兜底），绝不触发止损。
package quote

import (
	"context"

	"jingzhe-trader/internal/model"
)

// Quote 单标的实时报价（金额统一为 model.Fen）。
type Quote struct {
	TsCode     string
	Price      model.Fen // 最新价
	PreClose   model.Fen
	Open       model.Fen
	High       model.Fen
	Low        model.Fen
	ServerTime string
	Source     string // gotdx / tencent / cache
}

// Source 实时行情源接口（可替换为 gotdx / tencent / mock）。
type Source interface {
	// Fetch 拉取一批标的实时报价。返回 map[ts_code]Quote。
	// 失败时返回尽可能多的有效价（含缓存兜底），不触发止损。
	Fetch(ctx context.Context, tsCodes []string) (map[string]Quote, error)
}
