package dataloader

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"
	"jingzhe-trader/internal/market"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
)

// 场内 ETF 日线接入（科技 ETF 补口的数据面）。
//
// 落库沿用个股那套双列语义，一行都不特殊：
//
//	Close    = adjFen(RawClose × adj_factor)  —— 前复权，因子/均线/动量的口径
//	RawClose = 未复权真实成交价（分）        —— 一手成本、持仓市值、指令单参考价的口径
//	VolLot   = 成交量（手，1 手 = 100 份）
//
// 复权在这里不是可选项。实测 515050.SH 在 20260513 做了 1:3 份额折算：未复权收盘
// 3.33 → 1.156（−65.3%），复权后 3.33 → 3.468（+4.1%）。按未复权序列算出来的
// "4 年日σ 3.07% / 最大回撤 −73.5%" 全是折算造成的假象（真实值 2.39% / −44.1%）。
// 而一手成本必须用未复权价：按 3.47 元报价去算"一手 347 元"，用户就按一张下不了的单子下单。
//
// 与个股链的差别只在"缺因子怎么办"：个股当日缺因子会让整批中止（adj_factor 与 daily
// 不同日 = 全市场截面不可信），ETF 是补口而不是主链，缺哪只跳哪只并出声，
// 不能因为 fund_adj 晚发布就把整条 evening_pipeline 掐掉（设计文档 §5 的"只降级不断链"）。

// ETFMinScaleYi 白名单标的的规模下限（亿元）。
//
// 刻意不做配置键：这一条是"防某日规模塌陷"的资格线，不是可调参数。给它一个键等于
// 给一个从没被单独调过、又直接影响标的存活的数字留出挑参空间（同设计文档 §5-7 的口径）。
const ETFMinScaleYi = 50.0

// shareLookbackDays 份额查询回看的自然日数：fd_share 偶有停更几天，留一周窗口
// 取最近一行，避免"份额没发布"被误判成"规模不足"。
const shareLookbackDays = 8

// SyncETFWindow 同步白名单 ETF 在 [start, end]（含）区间的日线，按 (ts_code, trade_date) 幂等覆盖。
//
// 每只 2 次接口（fund_daily 区间 + fund_adj 区间），与区间长度无关 —— 这就是设计文档
// 说"深历史比个股逐日回补便宜两个数量级"的原因（7 只 = 14 次调用，能一次拉满 4 年）。
// 返回写入行数；任一标的失败都会落一条 ETF_SYNC 告警并在 error 里汇总（调用方决定降级还是中止）。
func (d *Dataloader) SyncETFWindow(ctx context.Context, start, end string, codes []string) (int, error) {
	if len(codes) == 0 {
		return 0, nil
	}
	rc := d.store.MarketRepo()
	var (
		written int
		failed  []string
	)
	for _, code := range codes {
		bars, err := d.tushare.FundDailyRange(ctx, code, start, end)
		if err != nil {
			d.alertTushare(ctx, "fund_daily", fmt.Errorf("%s: %w", code, err))
			failed = append(failed, code+"(daily)")
			continue
		}
		if len(bars) == 0 {
			// 未上市/已退市/当日停牌都会走到这里：不是故障，出声即可，不写库。
			observability.S().Warnw("ETF 区间无日线", "ts_code", code, "start", start, "end", end)
			continue
		}
		adjs, err := d.tushare.FundAdj(ctx, code, start, end)
		if err != nil {
			d.alertTushare(ctx, "fund_adj", fmt.Errorf("%s: %w", code, err))
			failed = append(failed, code+"(adj)")
			continue
		}
		factors := make(map[string]float64, len(adjs))
		for _, a := range adjs {
			factors[a.TradeDate] = a.AdjFactor
		}
		out, noAdj := applyAdj(bars, factors)
		if len(noAdj) > 0 {
			d.raiseAlert(ctx, "ETF_ADJ_MISSING", "ETF 缺复权因子，当日不写入",
				fmt.Sprintf("%s 区间 %s..%s 有 %d 根缺因子（前 5 个日期 %s）",
					code, start, end, len(noAdj), strings.Join(firstN(noAdj, 5), ",")))
		}
		for _, b := range out {
			if err := rc.UpsertBar(ctx, b); err != nil {
				return written, err
			}
		}
		written += len(out)
	}
	if len(failed) > 0 {
		return written, fmt.Errorf("ETF 日线同步失败的标的：%s", strings.Join(failed, ","))
	}
	return written, nil
}

// SyncETF 同步白名单 ETF 的当日日线，并顺带做一次规模校验。
//
// 挂在个股 daily 之后、同一交易日：同一条幂等写入路径，补跑与到点跑结果一致。
// 规模校验只出声不剔除（见 CheckETFScale）。
func (d *Dataloader) SyncETF(ctx context.Context, date string, codes []string) (int, error) {
	written, err := d.SyncETFWindow(ctx, date, date, codes)
	if err != nil {
		return written, err
	}
	if scaleErr := d.CheckETFScale(ctx, date, codes); scaleErr != nil {
		return written, scaleErr
	}
	return written, nil
}

// CheckETFScale 校验白名单标的的最新规模估算，低于 ETFMinScaleYi 落一条告警。
//
// 估算口径：fd_share(万份) × 1e4 × 当日未复权收盘(元)。用二级市场价格代替单位净值
// 实测差 0.36%（515050 20261008：160.99 亿 vs 净值口径 161.57 亿），而这条线的用途是
// 拦"清盘级别的塌陷"（50 亿 vs 常见 100~900 亿），量级差三位数，代得不影响判定。
//
// 为什么不直接剔掉：白名单是人工按 fund_basic.name 复核过的资产，规模塌陷改变的是
// "这只标的还该不该在池子里"这个事实判断 —— 那需要人确认并改配置，不是同步进程
// 半夜替用户做主。所以这里只把事实写进 run_trace，让链路核查/日报必须回答它。
func (d *Dataloader) CheckETFScale(ctx context.Context, date string, codes []string) error {
	if len(codes) == 0 {
		return nil
	}
	from := shiftDate(date, -shareLookbackDays)
	sr := d.store.ScreenRepo()
	var notes []string
	for _, code := range codes {
		shares, err := d.tushare.FundShare(ctx, code, from, date)
		if err != nil {
			return d.alertTushare(ctx, "fund_share", fmt.Errorf("%s: %w", code, err))
		}
		if len(shares) == 0 {
			notes = append(notes, fmt.Sprintf("%s 份额未发布（%s..%s），规模不可校验", code, from, date))
			continue
		}
		sort.SliceStable(shares, func(i, j int) bool { return shares[i].TradeDate < shares[j].TradeDate })
		last := shares[len(shares)-1]
		bar, err := sr.LatestBarAt(ctx, code, date)
		if err != nil {
			return fmt.Errorf("读取 %s 在 %s 的最近日线失败（规模校验）: %w", code, date, err)
		}
		if bar.RawClose <= 0 {
			notes = append(notes, fmt.Sprintf("%s 无有效收盘价，规模不可校验", code))
			continue
		}
		yi := last.FDShare * 1e4 * bar.RawClose.Float() / 1e8
		if yi < ETFMinScaleYi {
			notes = append(notes, fmt.Sprintf("%s 估算规模 %.2f 亿 < %.0f 亿（份额 %.2f 亿份@%s）",
				code, yi, ETFMinScaleYi, last.FDShare/1e4, last.TradeDate))
		}
	}
	if len(notes) > 0 {
		d.raiseAlert(ctx, "ETF_SCALE", "ETF 白名单规模/份额校验异常", strings.Join(notes, "；"))
	}
	observability.L().Info("ETF 规模校验完成", zap.String("date", date),
		zap.Int("codes", len(codes)), zap.Int("notes", len(notes)))
	return nil
}

// applyAdj 按"日期 → 因子"映射就地复权（与 syncOneDay 的个股路径同一约定）。
//
// 单独抽出来是为了让它可测：份额折算是历史事实，回归用例不该依赖网络。
// 返回 (可写入的行, 缺因子的日期)。缺因子的行一律不写 —— 与个股那侧同一个理由：
// 同一根序列里一半是复权价一半是原始价，下游拿它算止损就会凭空看到一次跳水。
func applyAdj(bars []model.Bar, factors map[string]float64) ([]model.Bar, []string) {
	out := make([]model.Bar, 0, len(bars))
	var missing []string
	for _, b := range bars {
		f := factors[b.TradeDate]
		if f == 0 {
			missing = append(missing, b.TradeDate)
			continue
		}
		raw := b.Close // 适配层给出的未复权收盘（分）
		b.Close = adjFen(raw, f)
		b.RawClose = raw
		out = append(out, b)
	}
	return out, missing
}

// shiftDate 按自然日偏移 YYYYMMDD（份额回看窗口用，不涉及交易日语义）。
func shiftDate(date string, days int) string {
	t, err := time.ParseInLocation("20060102", date, market.Loc)
	if err != nil {
		return date
	}
	return t.AddDate(0, 0, days).Format("20060102")
}
