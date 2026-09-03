// Package dataloader 数据接入编排（L3）：日历/日线/财务同步 + 校验 + 新鲜度门禁。
//
// 依赖方向（ARCHITECTURE §1）：dataloader 依赖 store（仓储）、tushare/quote（适配层）、
// model、market、observability；不直接触网（网络只在适配层）。
package dataloader

import (
	"context"
	"errors"
	"fmt"
	"time"

	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
	"go.uber.org/zap"
)

// majorIndices 大盘指数列表（index_daily 主源，单调用 ts_code 逗号拼接）。
var majorIndices = []string{
	"000001.SH", // 上证指数
	"000300.SH", // 沪深300
	"000905.SH", // 中证500
	"399001.SZ", // 深证成指
	"399006.SZ", // 创业板指
	"000016.SH", // 上证50
}

// Dataloader 数据接入编排器。
type Dataloader struct {
	store   *store.Store
	tushare *tushare.Client
}

// New 构造数据接入器。
func New(s *store.Store, tcli *tushare.Client) *Dataloader {
	return &Dataloader{store: s, tushare: tcli}
}

// SyncCalendar 同步交易日历（覆盖至 2099，确保未来 ≥365 开市日）。
// 跨上交所/深交所/北交所合并，避免单交易所口径缺口。
func (d *Dataloader) SyncCalendar(ctx context.Context) error {
	rc := d.store.MarketRepo()
	exchanges := []string{"SSE", "SZSE", "BSE"}
	total := 0
	for _, ex := range exchanges {
		rows, err := d.tushare.TradeCal(ctx, ex, "19900101", "20991231")
		if err != nil {
			return d.alertTushare(ctx, "trade_cal", err)
		}
		for _, r := range rows {
			if err := rc.UpsertCal(ctx, store.CalRow{
				CalDate:       r.CalDate,
				IsOpen:        r.IsOpen,
				PreTradeDate:  r.PreTradeDate,
				NextTradeDate: r.NextTradeDate,
				Exchange:      r.Exchange,
			}); err != nil {
				return err
			}
		}
		total += len(rows)
	}
	observability.L().Info("交易日历同步完成", zap.Int("exchanges", len(exchanges)), zap.Int("rows", total))

	// 前向补齐：Tushare trade_cal 仅返回至约 2027 年，远期为空。
	// 为保证调度/门禁能看到 ≥365 个未来开市日，按工作日（周一~周五）前向补齐至 2099-12-31。
	added, err := d.fillForwardCalendar(ctx)
	if err != nil {
		return fmt.Errorf("前向补齐交易日历失败: %w", err)
	}
	if added > 0 {
		observability.L().Info("交易日历前向补齐完成", zap.Int("added", added))
	}
	return nil
}

// fillForwardCalendar 从现有日历最大日期的次日起，按工作日（周一~周五）生成开市日，
// 补齐至 2099-12-31。Tushare 仅提供近未来交易日历，远期为空；补齐仅影响 2028 年以后的
// 工作日（真实近未来数据保持不变），用于保证调度/新鲜度门禁有足够长的未来开市日 horizon。
//
// 生成的行 Exchange 标记为 "FWD"（前向补齐），与 Tushare 真实数据（SSE/SZSE/BSE）可区分、可审计。
func (d *Dataloader) fillForwardCalendar(ctx context.Context) (int, error) {
	rc := d.store.MarketRepo()
	maxDate, err := rc.MaxCalDate(ctx)
	if err != nil {
		return 0, err
	}
	start, perr := time.ParseInLocation("20060102", maxDate, market.Loc)
	if perr != nil {
		start = time.Date(2027, 12, 31, 0, 0, 0, 0, market.Loc)
	}
	end := time.Date(2099, 12, 31, 0, 0, 0, 0, market.Loc)
	cur := start.AddDate(0, 0, 1)
	added := 0
	for cur.Before(end) {
		wd := cur.Weekday()
		if wd != time.Saturday && wd != time.Sunday {
			if err := rc.UpsertCal(ctx, store.CalRow{
				CalDate:  cur.Format("20060102"),
				IsOpen:   true,
				Exchange: "FWD",
			}); err != nil {
				return added, err
			}
			added++
		}
		cur = cur.AddDate(0, 0, 1)
	}
	return added, nil
}

// SyncDaily 同步指定交易日日线，并自动回补前 backDays 个交易日以修复缺口。
func (d *Dataloader) SyncDaily(ctx context.Context, tradeDate string, backDays int) error {
	rc := d.store.MarketRepo()
	days, err := rc.TradeDateList(ctx)
	if err != nil {
		return err
	}
	dates := pickBackDates(days, tradeDate, backDays)
	observability.L().Info("日线同步开始",
		zap.String("target", tradeDate), zap.Int("back_days", backDays), zap.Int("total", len(dates)))
	for _, dt := range dates {
		if err := d.syncOneDay(ctx, rc, dt); err != nil {
			return err
		}
		observability.L().Info("日线同步完成", zap.String("date", dt))
	}
	return nil
}

// pickBackDates 选取 tradeDate 及其之前最多 backDays 个已交易日（用于回补）。
//   - backDays==0：仅同步目标日当天（默认每日增量同步的正确语义）
//   - backDays>0 ：目标日 + 之前 backDays 个交易日，共 backDays+1 天
//   - backDays<0 ：全量回溯（截至目标日的全部已交易日，仅限一次性历史补数，慎用）
//
// 修复：原实现 backDays==0 时 start 保持 0 返回 1990 年至今全部日期，导致每次 daily
// 都重刷 35 年历史（数千次接口调用）。
func pickBackDates(days []string, tradeDate string, backDays int) []string {
	prior := make([]string, 0, len(days))
	for _, dd := range days {
		if dd <= tradeDate {
			prior = append(prior, dd)
		}
	}
	if len(prior) == 0 {
		return nil
	}
	if backDays < 0 {
		return prior // 全量回溯
	}
	count := backDays + 1 // 目标日 + backDays 个前置交易日
	if count > len(prior) {
		count = len(prior)
	}
	return prior[len(prior)-count:]
}

// syncOneDay 单交易日六接口快路径同步（各一次调用，全市场批量）。
func (d *Dataloader) syncOneDay(ctx context.Context, rc *store.MarketRepo, date string) error {
	bars, err := d.tushare.Daily(ctx, date)
	if err != nil {
		return d.alertTushare(ctx, "daily", err)
	}
	dbs, err := d.tushare.DailyBasic(ctx, date)
	if err != nil {
		return d.alertTushare(ctx, "daily_basic", err)
	}
	adjs, err := d.tushare.AdjFactor(ctx, date)
	if err != nil {
		return d.alertTushare(ctx, "adj_factor", err)
	}
	limits, err := d.tushare.StkLimit(ctx, date)
	if err != nil {
		return d.alertTushare(ctx, "stk_limit", err)
	}
	susp, err := d.tushare.Suspend(ctx, date)
	if err != nil {
		return d.alertTushare(ctx, "suspend_d", err)
	}
	idxs, err := d.tushare.IndexDaily(ctx, date, majorIndices)
	if err != nil {
		// index_daily 接口权限可选（40101/40203 等）：降级告警、跳过指数数据，不阻断主流程。
		var apiErr *tushare.APIError
		if errors.As(err, &apiErr) && apiErr.Kind == tushare.KindPermanent {
			observability.L().Warn("指数日线接口无权限，降级跳过",
				zap.String("api", "index_daily"), zap.Int("code", apiErr.Code), zap.String("msg", apiErr.Msg))
			idxs = nil
		} else {
			return d.alertTushare(ctx, "index_daily", err)
		}
	}
	mfs, err := d.tushare.MoneyFlow(ctx, date)
	if err != nil {
		return d.alertTushare(ctx, "moneyflow", err)
	}

	// 复权因子映射（前复权统一口径：Close/OHLC = RawClose × AdjFactor）
	adjMap := make(map[string]float64, len(adjs))
	for _, a := range adjs {
		adjMap[a.TsCode] = a.AdjFactor
	}

	for i := range bars {
		b := &bars[i]
		f := adjMap[b.TsCode]
		if f == 0 {
			f = 1.0 // 缺复权因子时视为未调整，保持原始价不变
		}
		rawClose := b.Close
		b.Open = adjFen(b.Open, f)
		b.High = adjFen(b.High, f)
		b.Low = adjFen(b.Low, f)
		b.PreClose = adjFen(b.PreClose, f)
		b.Close = adjFen(rawClose, f)
		b.RawClose = rawClose
		b.AdjFactor = f
		if err := rc.UpsertBar(ctx, *b); err != nil {
			return err
		}
	}
	for _, x := range dbs {
		if err := rc.UpsertDailyBasic(ctx, x); err != nil {
			return err
		}
	}
	for _, a := range adjs {
		if err := rc.UpsertAdjFactor(ctx, a.TsCode, a.TradeDate, a.AdjFactor); err != nil {
			return err
		}
	}
	for _, l := range limits {
		if err := rc.UpsertLimit(ctx, l); err != nil {
			return err
		}
	}
	for _, s := range susp {
		if err := rc.UpsertSuspend(ctx, s); err != nil {
			return err
		}
	}
	for _, x := range idxs {
		if err := rc.UpsertIndexDaily(ctx, x); err != nil {
			return err
		}
	}
	for _, m := range mfs {
		if err := rc.UpsertMoneyFlow(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// adjFen 按复权因子换算金额（分）。
func adjFen(raw model.Fen, f float64) model.Fen {
	return model.FromFloat(raw.Float() * f)
}

// raiseAlert 落一条告警（运维域）。
func (d *Dataloader) raiseAlert(ctx context.Context, code string, level model.AlertLevel, title, content string) {
	now := time.Now().In(market.Loc)
	_ = d.store.OpsRepo().RaiseAlert(ctx, model.AgentAlert{
		TradeDate: now.Format("20060102"),
		Source:    "dataloader",
		Level:     level,
		Code:      code,
		Title:     title,
		Content:   content,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// alertTushare 包装 Tushare 调用错误：若为永久错误（无权限/接口名错/积分不足），
// 直接落告警（含接口名+原因），不重试；并原样返回错误供上层决策。
func (d *Dataloader) alertTushare(ctx context.Context, apiName string, err error) error {
	var apiErr *tushare.APIError
	if errors.As(err, &apiErr) && apiErr.Kind == tushare.KindPermanent {
		d.raiseAlert(ctx, "TUSHARE_PERMANENT", model.AlertUrgent,
			fmt.Sprintf("Tushare 接口 %s 永久错误", apiName),
			fmt.Sprintf("接口=%s code=%d msg=%s", apiName, apiErr.Code, apiErr.Msg))
	}
	return err
}
