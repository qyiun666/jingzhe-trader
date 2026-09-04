package quote

import (
	"context"
	"fmt"
	"strings"

	"github.com/bensema/gotdx"
	"github.com/bensema/gotdx/types"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
)

// GotdxSource gotdx 实时行情源（全系统唯一实现，不再有 Source 接口：
// 备用源已删，也没有任何测试需要 mock 它 —— 行情只能通过真接口验证）。
type GotdxSource struct {
	client *gotdx.Client
}

// NewGotdxSource 构造 gotdx 行情源。
func NewGotdxSource() *GotdxSource {
	return &GotdxSource{client: gotdx.New()}
}

// Fetch 拉取实时报价：任一标的没取到价即整体失败。
//
// 没有备用源、没有缓存兜底：盘中拿不到当前价就只能报"取不到价"，
// 拿旧价继续跑等于用昨天的价格判断今天的止损，宁可不判断。
func (s *GotdxSource) Fetch(ctx context.Context, tsCodes []string) (map[string]Quote, error) {
	if len(tsCodes) == 0 {
		return map[string]Quote{}, nil
	}
	if err := s.connect(); err != nil {
		return nil, fmt.Errorf("gotdx 连接失败: %w", err)
	}
	markets, codes := splitMarkets(tsCodes)
	reply, err := s.client.GetSecurityQuotes(markets, codes)
	if err != nil {
		return nil, fmt.Errorf("gotdx 行情失败: %w", err)
	}
	res := make(map[string]Quote, len(tsCodes))
	for _, q := range reply.List {
		ts := normalizeCode(q.Code, q.Market)
		res[ts] = Quote{
			TsCode:     ts,
			Price:      model.FromFloat(q.Close),
			PreClose:   model.FromFloat(q.PreClose),
			Open:       model.FromFloat(q.Open),
			High:       model.FromFloat(q.High),
			Low:        model.FromFloat(q.Low),
			ServerTime: q.ServerTime,
			Source:     "gotdx",
		}
	}
	if missing := missingCodes(tsCodes, res); len(missing) > 0 {
		return nil, fmt.Errorf("gotdx 未返回 %d/%d 个标的的报价: %s",
			len(missing), len(tsCodes), strings.Join(missing, ","))
	}
	// 0 价同样算没拿到价：停牌、退市整理、代码写错都会以 0 出现，
	// 放过去等于这只持仓今天不判止损。
	var zero []string
	for _, c := range tsCodes {
		if res[c].Price <= 0 {
			zero = append(zero, c)
		}
	}
	if len(zero) > 0 {
		return nil, fmt.Errorf("gotdx 返回 %d 个 0 价标的（停牌或代码无效）: %s",
			len(zero), strings.Join(firstN(zero, 5), ","))
	}
	return res, nil
}

// missingCodes 返回请求了但没拿到报价的标的（按请求顺序）。
func missingCodes(requested []string, got map[string]Quote) []string {
	var out []string
	for _, c := range requested {
		if _, ok := got[c]; !ok {
			out = append(out, c)
		}
	}
	return out
}

// firstN 错误信息只列前 n 个代码，避免整仓停牌时把轨迹 detail 撑爆。
func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// connect 连接；默认节点不可达时探测并切到最快可达节点（同一个源，不是备用源）。
func (s *GotdxSource) connect() error {
	if _, err := s.client.Connect(); err == nil {
		return nil
	}
	if _, ferr := s.client.FastestHost(); ferr != nil {
		return fmt.Errorf("主节点不可达且最快节点探测失败: %w", ferr)
	}
	if _, cerr := s.client.Connect(); cerr != nil {
		return fmt.Errorf("切换最快节点后仍连接失败: %w", cerr)
	}
	observability.S().Warnw("gotdx 已切换到最快节点", "reason", "默认节点连接失败")
	return nil
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
