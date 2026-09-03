package quote

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bensema/gotdx"
	"github.com/bensema/gotdx/types"
	"jingzhe-trader/internal/model"
)

// GotdxSource gotdx 实时行情主源。
type GotdxSource struct {
	client   *gotdx.Client
	fallback Source // 降级备用源（腾讯）
	cacheMu  sync.Mutex
	cache    map[string]Quote
	onStale  func(code, reason string) // 行情过时（兜底缓存）告警回调
}

// NewGotdxSource 构造 gotdx 主源；fallback 为可选降级源（通常为腾讯）。
func NewGotdxSource(fallback Source) *GotdxSource {
	return &GotdxSource{
		client:   gotdx.New(),
		fallback: fallback,
		cache:    make(map[string]Quote),
	}
}

// SetStaleHook 设置行情过时（兜底缓存）告警回调。
func (s *GotdxSource) SetStaleHook(hook func(code, reason string)) {
	s.onStale = hook
}

// Fetch 拉取实时报价：主节点不可达自动切最快节点；失败降级腾讯；再失败返回缓存价（不触发止损）。
func (s *GotdxSource) Fetch(ctx context.Context, tsCodes []string) (map[string]Quote, error) {
	if err := s.connect(ctx); err != nil {
		return s.degrade(ctx, tsCodes, fmt.Sprintf("gotdx 连接失败: %v", err))
	}
	markets, codes := splitMarkets(tsCodes)
	reply, err := s.client.GetSecurityQuotes(markets, codes)
	if err != nil {
		return s.degrade(ctx, tsCodes, fmt.Sprintf("gotdx 行情失败: %v", err))
	}
	res := make(map[string]Quote, len(tsCodes))
	for _, q := range reply.List {
		ts := normalizeCode(q.Code, q.Market)
		qq := Quote{
			TsCode:     ts,
			Price:      model.FromFloat(q.Close),
			PreClose:   model.FromFloat(q.PreClose),
			Open:       model.FromFloat(q.Open),
			High:       model.FromFloat(q.High),
			Low:        model.FromFloat(q.Low),
			ServerTime: q.ServerTime,
			Source:     "gotdx",
		}
		res[ts] = qq
		s.cacheMu.Lock()
		s.cache[ts] = qq
		s.cacheMu.Unlock()
	}
	// 未返回的标的用缓存兜底
	s.fillFromCache(res, tsCodes)
	if len(res) == 0 {
		return s.degrade(ctx, tsCodes, "gotdx 返回空行情")
	}
	return res, nil
}

// connect 连接并在主节点不可达时自动探测切换最快节点。
func (s *GotdxSource) connect(ctx context.Context) error {
	if _, err := s.client.Connect(); err != nil {
		if _, ferr := s.client.FastestHost(); ferr == nil {
			// 重新连接会使用连接顺序中最快可达节点
			if _, cerr := s.client.Connect(); cerr == nil {
				return nil
			}
		}
		return err
	}
	return nil
}

// degrade 降级路径：先尝试备用源，再尝试缓存兜底。
func (s *GotdxSource) degrade(ctx context.Context, tsCodes []string, reason string) (map[string]Quote, error) {
	if s.fallback != nil {
		if r, ferr := s.fallback.Fetch(ctx, tsCodes); ferr == nil || len(r) > 0 {
			s.mergeCache(r)
			if s.onStale != nil {
				s.onStale("QUOTE_DEGRADED", reason)
			}
			return r, nil
		}
	}
	res := s.snapshotCache(tsCodes)
	if len(res) > 0 {
		if s.onStale != nil {
			s.onStale("QUOTE_STALE", reason)
		}
		return res, nil
	}
	return nil, fmt.Errorf("行情全部失败: %s", reason)
}

// fillFromCache 用缓存兜底补齐 gotdx 未返回的标的。
func (s *GotdxSource) fillFromCache(res map[string]Quote, tsCodes []string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for _, code := range tsCodes {
		if _, ok := res[code]; !ok {
			if c, ok2 := s.cache[code]; ok2 {
				c2 := c
				c2.Source = "cache"
				res[code] = c2
				if s.onStale != nil {
					s.onStale("QUOTE_STALE", fmt.Sprintf("标的 %s 使用缓存价", code))
				}
			}
		}
	}
}

func (s *GotdxSource) mergeCache(r map[string]Quote) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	for k, v := range r {
		s.cache[k] = v
	}
}

func (s *GotdxSource) snapshotCache(tsCodes []string) map[string]Quote {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	res := make(map[string]Quote, len(tsCodes))
	for _, code := range tsCodes {
		if c, ok := s.cache[code]; ok {
			c2 := c
			c2.Source = "cache"
			res[code] = c2
		}
	}
	return res
}

// splitMarkets 将 ts_code 拆分为 gotdx 的 market(uint8) 与 code(6位)。
func splitMarkets(tsCodes []string) ([]uint8, []string) {
	markets := make([]uint8, 0, len(tsCodes))
	codes := make([]string, 0, len(tsCodes))
	for _, tc := range tsCodes {
		m, c := parseTsCode(tc)
		markets = append(markets, m)
		codes = append(codes, c)
	}
	return markets, codes
}

// parseTsCode 解析 ts_code 为 gotdx market 与 6 位代码。
func parseTsCode(tc string) (uint8, string) {
	parts := strings.SplitN(tc, ".", 2)
	code := parts[0]
	suffix := ""
	if len(parts) == 2 {
		suffix = strings.ToUpper(parts[1])
	}
	switch suffix {
	case "SH":
		return uint8(types.MarketSH), code
	case "SZ":
		return uint8(types.MarketSZ), code
	case "BJ":
		return uint8(types.MarketBJ), code
	default:
		if len(code) > 0 {
			switch code[0] {
			case '6':
				return uint8(types.MarketSH), code
			case '0', '3':
				return uint8(types.MarketSZ), code
			case '8', '9':
				return uint8(types.MarketBJ), code
			}
		}
		return uint8(types.MarketSH), code
	}
}

// normalizeCode 由 gotdx 返回的 6 位代码 + market 还原为 ts_code（如 600519.SH）。
func normalizeCode(code string, market uint8) string {
	suffix := "SH"
	switch types.Market(market) {
	case types.MarketSZ:
		suffix = "SZ"
	case types.MarketBJ:
		suffix = "BJ"
	case types.MarketSH:
		suffix = "SH"
	}
	return code + "." + suffix
}
