package tushare

import (
	"context"

	"jingzhe-trader/internal/model"
)

// 场内 ETF 接口一族（fund_*）。与 api_market.go 同形：只解码实际入库的列，
// 价格在适配层经 ToModel* 完成 float→Fen 边界（§11.4），单位口径以实测为准。
//
// 权限与形态实测（2026-10-09，500 积分档，只读探针）：
//   - fund_daily：ts_code 必传（只给 trade_date 返回空），单码传区间一次取回整段
//     （515050 一次 1151 根，20220104→20261008）。close 单位**元**、vol 单位**手**
//     （1 手 = 100 份）、amount 单位**千元**。校验：close×vol×100 与 amount 相差 ≤3%
//     （差额是 VWAP 与收盘价之差，不是量纲错）。
//     amount 不入库：daily_bar 没有该列，而为"日均成交额"单独加列要动 store，
//     设计文档 §5-3 的判定是无改动。成交额由 raw_close×vol_lot 推算，同一只标的
//     同一天的两个口径实测相差 0.4%~2.6%，判定线是"亿元"量级，不会翻面。
//   - fund_adj：与个股 adj_factor 同形，但**按日全量返回**（515050 1151 根日线对应
//     1151 行因子，缺因子日数 0）。份额折算就体现在这里：515050 在 20260512→20260513
//     由 1.0 跳到 3.0（原始收盘 3.33→1.156，未复权看是 −65% 跳水，复权后是 +4%）。
//   - fund_share：fd_share 单位**万份**。规模 = fd_share×1e4×单位净值，实测 515050
//     20261008 为 161.57 亿（nav 0.9193）；用当日未复权收盘 0.916 代替净值估出
//     160.99 亿，偏差 0.36% —— 因此规模校验不必再接 fund_nav 第五个接口。
//   - fund_basic(market=E)：2968 行，**没有跟踪标的字段**，白名单只能人工维护，
//     且必须用 name 复核（实测 515220 是煤炭 ETF、561560 是电力公用事业 ETF，
//     按代码前缀猜会纳错标的）。

// FundBasicRow fund_basic 解码 DTO（只取资格校验需要的列）。
type FundBasicRow struct {
	TsCode     string `db:"ts_code"`
	Name       string `db:"name"`
	FundType   string `db:"fund_type"`
	FoundDate  string `db:"found_date"`
	DelistDate string `db:"delist_date"` // 空 = 未退市
	Manager    string `db:"manager"`
}

// FundShareRow fund_share 解码 DTO（fd_share 单位万份）。
type FundShareRow struct {
	TsCode    string  `db:"ts_code"`
	TradeDate string  `db:"trade_date"`
	FDShare   float64 `db:"fd_share"`
}

// FundBasic 拉取场内基金份额基础信息（market=E 一次返回全市场 2968 行）。
//
// 用途是白名单复核（名称 + 是否退市），不是自动发现：本接口没有跟踪标的字段，
// "哪些是科技 ETF"这个问题在这里无解，只能人工维护、按 name 逐只核过。
func (c *Client) FundBasic(ctx context.Context, market string) ([]FundBasicRow, error) {
	fields, items, err := c.Call(ctx, "fund_basic",
		map[string]interface{}{"market": market},
		"ts_code", "name", "fund_type", "found_date", "delist_date", "manager")
	if err != nil {
		return nil, err
	}
	var rows []FundBasicRow
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// FundDailyRange 单码一段区间的 ETF 日线（一次调用取回整段）。
//
// 传同一个日期给 start/end 即为单日同步语义（逐日增量与深历史回补共用这一条路径，
// 避免出现第二套"当日接口 + 区间接口"的分叉实现）。
// 返回的 Close 是**未复权**收盘（分），与个股 Daily 同一约定：复权由同步侧就地完成。
func (c *Client) FundDailyRange(ctx context.Context, tsCode, startDate, endDate string) ([]model.Bar, error) {
	fields, items, err := c.Call(ctx, "fund_daily",
		map[string]interface{}{"ts_code": tsCode, "start_date": startDate, "end_date": endDate},
		"ts_code", "trade_date", "close", "vol")
	if err != nil {
		return nil, err
	}
	var raw []RawBar
	if err := DecodeItems(fields, items, &raw); err != nil {
		return nil, err
	}
	out := make([]model.Bar, 0, len(raw))
	for _, r := range raw {
		out = append(out, ToModelBar(r))
	}
	return out, nil
}

// FundAdj 单码一段区间的复权因子（按日全量，与个股 AdjFactor 的逐日全市场口径不同）。
func (c *Client) FundAdj(ctx context.Context, tsCode, startDate, endDate string) ([]AdjFactorRow, error) {
	fields, items, err := c.Call(ctx, "fund_adj",
		map[string]interface{}{"ts_code": tsCode, "start_date": startDate, "end_date": endDate},
		"ts_code", "trade_date", "adj_factor")
	if err != nil {
		return nil, err
	}
	var rows []AdjFactorRow
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// FundShare 单码一段区间的份额（万份），供规模校验用。
func (c *Client) FundShare(ctx context.Context, tsCode, startDate, endDate string) ([]FundShareRow, error) {
	fields, items, err := c.Call(ctx, "fund_share",
		map[string]interface{}{"ts_code": tsCode, "start_date": startDate, "end_date": endDate},
		"ts_code", "trade_date", "fd_share")
	if err != nil {
		return nil, err
	}
	var rows []FundShareRow
	if err := DecodeItems(fields, items, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
