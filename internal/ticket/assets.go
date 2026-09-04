package ticket

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/model"
)

// Assets 当前账户资产：现金 + 持仓市值 + 总资产，全部实时推算，不落快照表。
//
//   - market_value 用 ≤tradeDate 的最近一根 raw_close（未复权真实价）× 持仓量现算；
//     停牌股自然取停牌前收盘，因为 LatestBarAt 是按 trade_date 倒序取第一根。
//   - cash 由"本金或现金锚点 − Σ成交"推算，见 Ledger.Cash。
//   - 无持仓时市值 0 是正常结果：不插值、不沿用昨日值。
//
// tradeDate 同时作为返回值的市值取价截止日，供调用方判断行情是否陈旧。
func (l *Ledger) Assets(ctx context.Context, tradeDate string) (model.Assets, error) {
	positions, err := l.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return model.Assets{}, err
	}
	var mv model.Fen
	count := 0
	for _, pos := range positions {
		if pos.TotalQty <= 0 {
			continue
		}
		bar, err := l.st.ScreenRepo().LatestBarAt(ctx, pos.TsCode, tradeDate)
		if err != nil {
			return model.Assets{}, fmt.Errorf("市值取价 %s 失败（≤%s 无日线）: %w", pos.TsCode, tradeDate, err)
		}
		mv += bar.RawClose.Mul(pos.TotalQty)
		count++
	}
	cash, err := l.Cash(ctx)
	if err != nil {
		return model.Assets{}, err
	}
	return model.Assets{
		TradeDate:     tradeDate,
		Cash:          cash,
		MarketValue:   mv,
		TotalAsset:    cash + mv,
		PositionCount: count,
	}, nil
}
