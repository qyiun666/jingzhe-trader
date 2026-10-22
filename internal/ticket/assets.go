package ticket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
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
	// 读锁覆盖"读持仓 → 逐只取价 → 读现金"整段：组合同步是逐行写，
	// 不挡的话可能读到一半新一半旧的持仓截面。cashLocked 避免递归 RLock。
	l.mu.RLock()
	defer l.mu.RUnlock()
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
			// 无日线不是数据库故障：长期停牌超过保留窗、退市、手工同步进来的
			// ETF/代码笔误都会走到这里。以前整条资产读取硬失败，会连带
			// 风控参数、早盘计划、日报全部失败；现在按成本价回退并显式留痕。
			if errors.Is(err, sql.ErrNoRows) {
				observability.S().Warnw("持仓无可用日线，市值回退成本价",
					"ts_code", pos.TsCode, "date", tradeDate,
					"qty", int64(pos.TotalQty), "cost_price", int64(pos.CostPrice))
				l.recordAssetFallback(ctx, tradeDate, pos)
				mv += pos.CostPrice.Mul(pos.TotalQty)
				count++
				continue
			}
			return model.Assets{}, fmt.Errorf("市值取价 %s 失败（≤%s 无日线）: %w", pos.TsCode, tradeDate, err)
		}
		mv += bar.RawClose.Mul(pos.TotalQty)
		count++
	}
	cash, err := l.cashLocked(ctx)
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

// recordAssetFallback 把"持仓无日线、市值按成本价回退"落一行轨迹。
// 读取资产是高频路径，按 (trade_date, subject) 幂等覆盖，同一只票一天一行；
// 写失败只记日志，不能反过来把资产读取打挂。
func (l *Ledger) recordAssetFallback(ctx context.Context, tradeDate string, pos model.Position) {
	err := l.st.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: tradeDate,
		Subject:   model.TraceAlert("ASSET_NO_BAR:" + pos.TsCode),
		Outcome:   model.TracePartial,
		Detail: fmt.Sprintf("%s 持仓 %d 股在 ≤%s 无日线，市值按成本价 %.2f 元回退（非数据库故障）",
			pos.TsCode, int64(pos.TotalQty), tradeDate, pos.CostPrice.Float()),
		At: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		observability.S().Errorw("写持仓无日线回退轨迹失败", "ts_code", pos.TsCode, "err", err.Error())
	}
}
