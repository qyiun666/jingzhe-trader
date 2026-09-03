package dataloader

import (
	"context"
	"fmt"

	"jingzhe-trader/internal/model"
)

// FinaVisibleAsOf point-in-time 过滤：仅保留公告日 ≤ asOf 的财报，杜绝前视偏差。
//
// 选股/回测必须经由本函数读取财报，禁止裸查询 fina_indicator（验收 #7：
// 捏造 ann_date > 今日 的假财报，经此过滤后选股器读不到，即无前视）。
func FinaVisibleAsOf(rows []model.FinaIndicator, asOf string) []model.FinaIndicator {
	out := make([]model.FinaIndicator, 0, len(rows))
	for _, r := range rows {
		if r.AnnDate != "" && r.AnnDate > asOf {
			continue
		}
		out = append(out, r)
	}
	return out
}

// CheckAdjFactor 复权一致性校验：期望 Close == round(RawClose × AdjFactor)。
// 不一致视为脏数据，触发 BAR_ANOMALY 告警，返回异常 ts_code 列表（验收 #8）。
func (d *Dataloader) CheckAdjFactor(ctx context.Context, tradeDate string) ([]string, error) {
	bars, err := d.loadBars(ctx, tradeDate)
	if err != nil {
		return nil, err
	}
	var anomalies []string
	for _, b := range bars {
		expected := model.FromFloat(b.RawClose.Float() * b.AdjFactor)
		if expected != b.Close {
			anomalies = append(anomalies, b.TsCode)
			d.raiseAlert(ctx, "BAR_ANOMALY", model.AlertWarning,
				fmt.Sprintf("复权价不一致 %s/%s", b.TsCode, tradeDate),
				fmt.Sprintf("close=%d raw_close=%d adj_factor=%.6f expected=%d",
					int64(b.Close), int64(b.RawClose), b.AdjFactor, int64(expected)))
		}
	}
	return anomalies, nil
}

// loadBars 读取指定交易日全部日线（业务层只读，不触网）。
func (d *Dataloader) loadBars(ctx context.Context, tradeDate string) ([]model.Bar, error) {
	var bars []model.Bar
	const q = `SELECT ts_code, trade_date, open, high, low, close, pre_close, pct_chg, vol_lot, amount_k, adj_factor, raw_close
		FROM daily_bar WHERE trade_date = ?`
	if err := d.store.ReadDB().SelectContext(ctx, &bars, q, tradeDate); err != nil {
		return nil, fmt.Errorf("读取日线 %s 失败: %w", tradeDate, err)
	}
	return bars, nil
}
