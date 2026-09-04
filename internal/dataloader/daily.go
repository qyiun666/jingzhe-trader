// Package dataloader 数据接入编排（L3）：日历/日线/财务同步 + 校验 + 新鲜度门禁。
//
// 依赖方向（ARCHITECTURE §1）：dataloader 依赖 store（仓储）、tushare/quote（适配层）、
// model、market、observability；不直接触网（网络只在适配层）。
package dataloader

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
	"jingzhe-trader/internal/tushare"
)

// syncIndex 每日同步的大盘指数：只有这一根有读者（买入闸门 + 新鲜度门禁 IndexRows）。
// 其余指数代码既没有筛选条件消费也没有邮件展示，每天多拉 N 次接口换零读者没有意义。
var syncIndex = []string{store.MarketIndex}

// SyncStockBasics 同步在市股票清单（名称/行业/上市日/ST）：1 次调用返回全市场。
//
// 板块筛与资格筛都读 stock_basic，退市与行业变更只能靠这里带上；
// 曾经它挂在财务慢路径里，那条链路删掉后本表没有任何刷新方，会永久冻结。
func (d *Dataloader) SyncStockBasics(ctx context.Context) error {
	rows, err := d.tushare.StockBasic(ctx)
	if err != nil {
		return d.alertTushare(ctx, "stock_basic", err)
	}
	rc := d.store.MarketRepo()
	for i := range rows {
		if err := rc.UpsertStockBasic(ctx, rows[i]); err != nil {
			return err
		}
	}
	observability.L().Info("股票基础信息同步完成", zap.Int("rows", len(rows)))
	return nil
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

// SyncCalendar 同步交易日历：只拉滚动窗（今天往前 calLookback、往后 calLookahead）。
// 全项目最深的日历回看是 20 个交易日与当季起点，未来只需 ≥60 个开市日，
// 把 1990 年以来的每一天都存下来（32,310 行）没有任何读者，还会让 TradeDateList 每次读全表。
// 跨上交所/深交所/北交所合并，避免单交易所口径缺口。
func (d *Dataloader) SyncCalendar(ctx context.Context) error {
	const (
		calLookback  = -3 * 365 // 往前 3 年（覆盖季度起点与历史复现）
		calLookahead = 2 * 365  // 往后 2 年
	)
	rc := d.store.MarketRepo()
	now := time.Now().In(market.Loc)
	from := now.AddDate(0, 0, calLookback).Format("20060102")
	to := now.AddDate(0, 0, calLookahead).Format("20060102")
	exchanges := []string{"SSE", "SZSE", "BSE"}
	total := 0
	for _, ex := range exchanges {
		rows, err := d.tushare.TradeCal(ctx, ex, from, to)
		if err != nil {
			return d.alertTushare(ctx, "trade_cal", err)
		}
		for _, r := range rows {
			if err := rc.UpsertCal(ctx, store.CalRow{CalDate: r.CalDate, IsOpen: r.IsOpen}); err != nil {
				return err
			}
		}
		total += len(rows)
	}
	trimmed, err := rc.TrimCal(ctx, from, to)
	if err != nil {
		return err
	}
	observability.L().Info("交易日历同步完成",
		zap.String("from", from), zap.String("to", to),
		zap.Int("exchanges", len(exchanges)), zap.Int("rows", total), zap.Int("trimmed_out_of_window", trimmed))

	// 前向补齐：Tushare trade_cal 只给到约 2027 年，远期为空。真实行不足未来开市日阈值时按工作日补齐。
	added, err := d.fillForwardCalendar(ctx)
	if err != nil {
		return fmt.Errorf("前向补齐交易日历失败: %w", err)
	}
	if added > 0 {
		observability.L().Info("交易日历前向补齐完成", zap.Int("added", added))
	}
	return nil
}

// fillForwardCalendar 只在真实日历余量不足时，从真实最大日期的次日起按工作日（周一~周五）
// 补齐到 forwardOpenDays 个未来开市日，并把旧的合成行（synthetic=1）整批清掉重建。
//
// 刻意不是一路造到 2099：合成行不含节假日，造得越远，把休市日误判成交易日的时间窗越长，
// 而调度与门禁只需要"未来 ≥30 个开市日"（阈值 30，这里留 2 倍余量）。
func (d *Dataloader) fillForwardCalendar(ctx context.Context) (int, error) {
	const forwardOpenDays = 60
	rc := d.store.MarketRepo()
	if _, err := rc.DeleteSyntheticCal(ctx); err != nil {
		return 0, err
	}
	today := time.Now().In(market.Loc).Format("20060102")
	future, err := rc.CountFutureTradeDays(ctx, today)
	if err != nil {
		return 0, err
	}
	need := forwardOpenDays - future
	if need <= 0 {
		return 0, nil
	}
	maxDate, err := rc.MaxCalDate(ctx)
	if err != nil {
		return 0, err
	}
	cur, perr := time.ParseInLocation("20060102", maxDate, market.Loc)
	if perr != nil {
		cur = time.Now().In(market.Loc)
	}
	added := 0
	for added < need {
		cur = cur.AddDate(0, 0, 1)
		wd := cur.Weekday()
		if wd == time.Saturday || wd == time.Sunday {
			continue
		}
		if err := rc.UpsertCal(ctx, store.CalRow{
			CalDate: cur.Format("20060102"),
			IsOpen:  true,
			// 合成行必须可识别：它不含节假日，真实日历续上以后要整批清掉重建。
			Synthetic: true,
		}); err != nil {
			return added, err
		}
		added++
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
	if len(dates) == 0 {
		return fmt.Errorf("日历里 %s 及之前没有任何交易日（日历行数 %d），先跑 calendar 再同步日线",
			tradeDate, len(days))
	}
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

// syncOneDay 单交易日五接口快路径同步（各一次调用，全市场批量）：
// daily / daily_basic / adj_factor / suspend_d / index_daily。
// 不拉 stk_limit 与 moneyflow：前者涨跌停判定无消费者、后者五因子模型不消费，落库表已删除。
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
	susp, err := d.tushare.Suspend(ctx, date)
	if err != nil {
		return d.alertTushare(ctx, "suspend_d", err)
	}
	idxs, err := d.tushare.IndexDaily(ctx, date, syncIndex)
	if err != nil {
		return d.alertTushare(ctx, "index_daily", err)
	}

	// 复权因子映射（前复权统一口径：Close/OHLC = RawClose × AdjFactor）
	adjMap := make(map[string]float64, len(adjs))
	for _, a := range adjs {
		adjMap[a.TsCode] = a.AdjFactor
	}

	var noAdj []string
	for i := range bars {
		b := &bars[i]
		f := adjMap[b.TsCode]
		if f == 0 {
			// 缺复权因子就不能写：当日写原始价、历史写前复权价，同一根序列里
			// 会凭空出现一次"跳水"，而下游拿它算止损与涨跌幅。
			noAdj = append(noAdj, b.TsCode)
			continue
		}
		rawClose := b.Close
		b.Close = adjFen(rawClose, f)
		b.RawClose = rawClose
		if err := rc.UpsertBar(ctx, *b); err != nil {
			return err
		}
	}
	if len(noAdj) > 0 {
		return fmt.Errorf("%s 有 %d 只个股缺复权因子（前 5 只 %s），adj_factor 与 daily 不同日",
			date, len(noAdj), strings.Join(firstN(noAdj, 5), ","))
	}
	if err := rc.SaveValuation(ctx, date, dbs); err != nil {
		return err
	}
	if err := rc.SaveSuspended(ctx, date, susp); err != nil {
		return err
	}
	for _, x := range idxs {
		if err := rc.UpsertBar(ctx, model.Bar{TsCode: x.TsCode, TradeDate: x.TradeDate, Close: x.Close, VolLot: 0, RawClose: 0}); err != nil {
			return err
		}
	}
	return nil
}

// adjFen 按复权因子换算金额（分）。
func adjFen(raw model.Fen, f float64) model.Fen {
	return model.FromFloat(raw.Float() * f)
}

// firstN 取前 n 个元素（错误信息用：缺失面再大也不能把 detail 撑爆）。
func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// raiseAlert 落一条失败轨迹。写失败只记日志，不打断数据同步主流程。
func (d *Dataloader) raiseAlert(ctx context.Context, code, title, content string) {
	now := time.Now().In(market.Loc)
	err := d.store.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: now.Format("20060102"),
		Subject:   model.TraceAlert(code),
		Outcome:   model.TraceFail,
		Detail:    fmt.Sprintf("%s: %s", title, content),
		At:        now.UTC().Format(time.RFC3339),
	})
	if err != nil {
		observability.S().Errorw("写告警轨迹失败", "code", code, "err", err)
	}
}

// alertTushare 包装 Tushare 调用错误：若为永久错误（无权限/接口名错/积分不足），
// 直接落告警（含接口名+原因），不重试；并原样返回错误供上层决策。
func (d *Dataloader) alertTushare(ctx context.Context, apiName string, err error) error {
	var apiErr *tushare.APIError
	if errors.As(err, &apiErr) && apiErr.Kind == tushare.KindPermanent {
		d.raiseAlert(ctx, "TUSHARE_PERMANENT",
			fmt.Sprintf("Tushare 接口 %s 永久错误", apiName),
			fmt.Sprintf("接口=%s code=%d msg=%s", apiName, apiErr.Code, apiErr.Msg))
	}
	return err
}
