package ticket

import (
	"context"
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
)

// TakeSnapshot 生成账户日终快照（验收 #13）：
//   - market_value 用库内最新收盘价现算（position × 最近 raw_close）；
//   - 停牌股自然取停牌前收盘（LatestBarAt 按 trade_date ≤ 当日倒序取第一根）；
//   - cash 由成交历史推算；total_asset = cash + market_value。
//
// 快照不插值、不沿用昨日值：无持仓时 market_value=0 属正常产出。
func (l *Ledger) TakeSnapshot(ctx context.Context, tradeDate string, gear model.Gear, profitLock bool) (model.AccountSnapshot, error) {
	positions, err := l.st.TradeRepo().ListPositions(ctx)
	if err != nil {
		return model.AccountSnapshot{}, err
	}
	var mv model.Fen
	count := 0
	for _, pos := range positions {
		if pos.TotalQty <= 0 {
			continue
		}
		bar, err := l.st.ScreenRepo().LatestBarAt(ctx, pos.TsCode, tradeDate)
		if err != nil {
			return model.AccountSnapshot{}, fmt.Errorf("快照取价 %s 失败（≤%s 无日线）: %w", pos.TsCode, tradeDate, err)
		}
		mv += bar.RawClose.Mul(pos.TotalQty)
		count++
	}
	cash, err := l.Cash(ctx)
	if err != nil {
		return model.AccountSnapshot{}, err
	}
	sn := model.AccountSnapshot{
		TradeDate:     tradeDate,
		Cash:          cash,
		MarketValue:   mv,
		TotalAsset:    cash + mv,
		PositionCount: count,
		Gear:          gear,
		ProfitLock:    profitLock,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	if err := l.st.TradeRepo().UpsertSnapshot(ctx, sn); err != nil {
		return model.AccountSnapshot{}, err
	}
	return sn, nil
}
