package screener

// ETF 小漏斗：弱势试探期为"科技敞口买不起一手"这个硬缺口开的补口。
//
// 为什么不并入个股漏斗当一级来处理（设计文档 §2 的三条依据，落到代码上是两条）：
//  1. 个股的四项因子分全部是 PercentileRank 横截面百分位（基数 ~5500 只）。ETF 没有
//     PE/PB/流通市值/换手率，注入这个截面要么按 NaN→50 占位（污染中位），要么占据
//     极端低波/低换手分位（扭曲全部个股分数）——两者都会改变已经验证过的个股选股结果。
//  2. ETF 之间实测没有横截面信息：13 只科技 ETF 前复权 20 日动量 → 未来 20 日
//     RankIC 全样本 −0.019，前后两半分别 −0.038 / +0.001（符号相反），池内两两相关
//     最高 0.985。给它排一名二名就是样本内挑参，所以本漏斗**不做任何排名**。
//
// 因此这里只有两级：白名单资格（数据完整性）→ 流动性与一手预算，
// 剩下的"买哪只、买多少"交给同一个 LLM 评审。

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// etfSlugPool / etfSlugLiq 两级漏斗的 slug（进日志与 run_trace 的口径名）。
const (
	etfSlugPool = "etf_pool"
	etfSlugLiq  = "etf_liq"

	reasonEtfNoBar    = "窗口日线不足"
	reasonEtfStale    = "当日无报价"
	reasonEtfIlliquid = "日均成交额不足"
	reasonEtfNoName   = "白名单缺少名称"
	reasonEtfBadCode  = "代码不是场内基金段"
)

// ETFRepo 本漏斗需要的最小读取面（*store.ScreenRepo 满足；测试用桩替换）。
type ETFRepo interface {
	WindowDates(ctx context.Context, beforeInclusive string, n int) ([]string, error)
	BarsForCodes(ctx context.Context, codes, dates []string) ([]store.ClosePoint, error)
}

// ETFInstrument 白名单条目：代码 + 名称 + 跟踪方向。
//
// 名称必须由人工按 fund_basic.name 复核后写进配置：实测按代码前缀猜会踩空
// （515220 是煤炭 ETF、561560 是电力公用事业 ETF），而名称要进指令单与邮件，
// 跟踪方向要进 LLM 的标的说明——猜错的名称会让用户按一张错误的单子下单。
type ETFInstrument struct {
	TsCode string
	Name   string
	Track  string // 跟踪方向（半导体设备/科创50/人工智能…），落 Candidate.Industry
}

// etfWhitelistItems screen.etf_whitelist 的唯一切分点（逗号/分号/换行/制表/空格）。
//
// 有意的唯一实现：白名单字符串在三个地方要用（漏斗取整条、同步侧只取代码列、
// 装配期做严格校验），任何一处自己 split 就会在"分隔符/大小写/裸代码写法"上漂移，
// 而同步侧与漏斗侧一旦读到不同的代码集，症状就是"日线明明同步了却整天窗口不足"。
func etfWhitelistItems(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\t' || r == ' '
	})
}

// ParseETFWhitelist 解析 screen.etf_whitelist。
//
// 容许三种写法，逐级降级而不是报错：`code` / `code:name` / `code:name:track`。
// 只给代码时名称留空，由 RunETF 在资格级把它剔掉——宁可当日少一只候选，
// 也不能让一张写着"未知标的"的指令单发到用户手里。
func ParseETFWhitelist(raw string) []ETFInstrument {
	var out []ETFInstrument
	seen := map[string]bool{}
	for _, item := range etfWhitelistItems(raw) {
		parts := strings.Split(item, ":")
		code := normalizeEtfCode(parts[0])
		if code == "" || seen[code] {
			// 同一只标的写两遍（大小写或后缀写法不同也算）只留第一条：
			// 重复条目会让同一个 LLM 批次里出现两张同一标的的候选，等于把权重双算。
			continue
		}
		seen[code] = true
		it := ETFInstrument{TsCode: code}
		if len(parts) > 1 {
			it.Name = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			it.Track = strings.TrimSpace(parts[2])
		}
		if it.Track == "" {
			it.Track = it.Name // 没给跟踪方向时至少让"行业"一栏不是空的
		}
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TsCode < out[j].TsCode })
	return out
}

// ETFWhitelistCodes 白名单声明的代码集，供数据链路按代码同步 fund_daily。
//
// 输出升序且已去重、已补交易所后缀 —— 与 ParseETFWhitelist 给出的是同一份标的清单。
// 同步侧（工程的 ETF 日线链路）请调这一行，不要再自行 split：两处切分迟早会在
// 分隔符或裸代码写法上漂移，而漂移的症状是"日线同步了却整天窗口不足"，最难归因。
func ETFWhitelistCodes(raw string) []string {
	pool := ParseETFWhitelist(raw)
	codes := make([]string, 0, len(pool))
	for _, in := range pool {
		codes = append(codes, in.TsCode)
	}
	return codes
}

// etfCodeRE 场内标的代码形态：6 位数字 + 交易所后缀（归一化之后的形态）。
var etfCodeRE = regexp.MustCompile(`^\d{6}\.(SH|SZ|BJ)$`)

// etfCodeSegmentOK 代码段与交易所必须自洽：上海场内基金是 5 开头、深圳是 1 开头。
//
// 这条把"个股被误写进 ETF 白名单"挡在启动期。上海个股 6 开头、深圳个股 0 开头、
// 创业板 3 开头，与场内基金的 5/1 段不重叠 —— 这正是 normalizeEtfCode 敢按首位补后缀
// 的同一个前提，所以把它从"归一化的隐含假设"升级成校验的硬条件。
// 挡它的理由不是洁癖：个股在 daily_bar 里本来就有日线，误写进来的 600908.SH 会
// 一路通过流动性与一手预算两级，然后以"ETF"的身份进 LLM 批次和指令单——
// 而 ETF 那套证据（无 PE/PB、免印花税、100 份起）对它是错的。
// BJ 段只验形态不验段位：北交所场内基金的首位段没有实测依据，硬猜会把合法标的挡在门外。
func etfCodeSegmentOK(code string) bool {
	if len(code) < 7 {
		return false // 形态没对上时段位无从谈起（调用方已先跑 etfCodeRE，这里只是不留 panic 面）
	}
	switch suffix := code[len(code)-3:]; suffix {
	case ".SH":
		return code[0] == '5'
	case ".SZ":
		return code[0] == '1'
	default: // .BJ
		return true
	}
}

// etfCodeOK 形态 + 段位都自洽才算场内基金代码（RunETF 资格级用的合并判据）。
//
// 不能只看段位：裸代码"600908"（个股，归一化不加后缀）取末两位比的是"08"，
// 落进 default 分支会被放过。形态检查在前，段位检查才有意义。
func etfCodeOK(code string) bool {
	return etfCodeRE.MatchString(code) && etfCodeSegmentOK(code)
}

// ValidateETFWhitelist 白名单的装配期严格校验（组合根 app.validateETF 调这一行）。
//
// enabled 决定严格程度，与运行期的实际风险对齐：
//   - 无论开关：代码形态/段位非法、重复标的都拒绝启动。代码写错不会报错，fund_daily 只会
//     返回空，于是整条 ETF 链静默地一只候选都没有 —— 那种错运行期看不见，只能在启动
//     那一下拦（ETF 链的默认失败形态是"静默 0 候选"，比启动失败危险得多）。
//   - 开闸时：额外要求标的池非空、每条都带人工复核过的名称。名称要进指令单与邮件，
//     缺名称的那张单子会把用户导向一只他认不出的标的。
//   - 关闸时（出厂默认）：残缺白名单只是一份"还没启用的配置"，不该让今天的进程起不来。
//
// 裸六位代码是合法写法（normalizeEtfCode 补后缀），BJ 段留开：场内 ETF 里确有北交所
// 品种，而现在白名单没有 —— 校验放宽一格不会让任何错静默，因为代码最终要和
// daily_bar 里 Tushare 的键对上，对不上就在资格级被剔掉并点名原因。
func ValidateETFWhitelist(raw string, enabled bool) error {
	seen := map[string]bool{}
	n := 0
	for _, item := range etfWhitelistItems(raw) {
		parts := strings.Split(item, ":")
		code := normalizeEtfCode(parts[0])
		if !etfCodeRE.MatchString(code) {
			return fmt.Errorf("screen.etf_whitelist 条目 %q 的代码不合法（应为 6 位数字 + .SH/.SZ/.BJ）", item)
		}
		if !etfCodeSegmentOK(code) {
			return fmt.Errorf("screen.etf_whitelist 条目 %q 的代码段与交易所不符（上海场内基金 5 开头、深圳 1 开头）", item)
		}
		if seen[code] {
			return fmt.Errorf("screen.etf_whitelist 代码 %s 重复（同一标的会在同一次评审里出现两次）", code)
		}
		seen[code] = true
		n++
		if enabled && (len(parts) < 2 || strings.TrimSpace(parts[1]) == "") {
			return fmt.Errorf("screen.etf_whitelist 条目 %q 缺少名称：名称要进指令单与邮件，必须由人工按 fund_basic.name 复核过", item)
		}
	}
	if enabled && n == 0 {
		return fmt.Errorf("screen.etf_enabled=true 但 screen.etf_whitelist 为空：启用补口却没有标的池")
	}
	return nil
}

// normalizeEtfCode 把裸六位代码补成 daily_bar 用的带交易所后缀形态。
//
// 白名单是人和配置里写的裸代码（588000），日线表里的键是 Tushare 形态（588000.SH）；
// 不做这一步的话每个条目都"窗口日线不足"，看起来像数据没同步，实际是格式没对上。
// 场内 ETF 的代码段与交易所一一对应且与个股段不重叠：5xxxxx 归上交所、1xxxxx 归深交所
// （个股是 6/0/3 开头），所以这条归一化不会把某只股票误认成 ETF。
// 已带后缀的原样保留，无法归一化的写法也保留（让资格级把它剔掉并点名原因）。
func normalizeEtfCode(raw string) string {
	code := strings.ToUpper(strings.TrimSpace(raw))
	if code == "" {
		return ""
	}
	if strings.Contains(code, ".") {
		return code
	}
	if len(code) != 6 {
		return code
	}
	switch code[0] {
	case '5':
		return code + ".SH"
	case '1':
		return code + ".SZ"
	}
	return code
}

// ETFOptions ETF 漏斗的运行参数（组合根从 config screen.etf_* 构建后注入）。
type ETFOptions struct {
	Enabled bool
	Pool    []ETFInstrument
	// MinAvgAmountFen 窗口日均成交额下限（分）。来自 screen.etf_min_avg_amount_yi。
	MinAvgAmountFen model.Fen
	// Window 流动性窗口（交易日）；<=0 取个股因子窗口 momentumBars，两处共用一个深度，
	// 避免出现第二条需要单独维护的均线/窗口常量。
	Window int
}

// ETFStage 一级漏斗的进出计数（与个股漏斗的 StageStat 同形，展示层可共用摘要）。
type ETFStage = StageStat

// ETFReport 一次 ETF 漏斗的产出（内存，不落库）。
type ETFReport struct {
	TradeDate  string
	Candidates []model.Candidate
	Stages     []ETFStage
	Notes      []string
}

// Empty 白名单非空却一只都没过两级 —— 调用方据此出声（数据没到位 vs 本来就不该买）。
func (r *ETFReport) Empty() bool { return len(r.Candidates) == 0 }

// Summary 两级漏斗的可读摘要。
func (r *ETFReport) Summary() string { return funnelSummary(r.Stages) }

// RunETF 跑 ETF 小漏斗：白名单资格 → 流动性与一手预算 → 候选（不排名）。
//
// 只有 store 读取失败或交易日历不足窗口根数才返回 error；单只标的缺数据一律按
// 淘汰计数处理。这个区分是有意的：ETF 日线是补口而不是主链，某天 fund_daily
// 没发布或没同步成功，正确反应是"当日少一个候选来源"并把原因写进 Notes，
// 而不是把整条 evening_pipeline 一起掐掉。
func RunETF(ctx context.Context, repo ETFRepo, opts ETFOptions, budget Budget, tradeDate string) (*ETFReport, error) {
	rep := &ETFReport{TradeDate: tradeDate}
	if len(opts.Pool) == 0 {
		rep.Notes = append(rep.Notes, "ETF 白名单为空，本漏斗跳过")
		return rep, nil
	}
	window := opts.Window
	if window <= 0 {
		window = momentumBars
	}
	dates, err := repo.WindowDates(ctx, tradeDate, window)
	if err != nil {
		return rep, err
	}
	if len(dates) < window {
		return rep, fmt.Errorf("交易日历在 %s 前只有 %d 个交易日，不足 ETF 流动性窗口 %d 根",
			tradeDate, len(dates), window)
	}
	codes := make([]string, 0, len(opts.Pool))
	byCode := make(map[string]ETFInstrument, len(opts.Pool))
	for _, in := range opts.Pool {
		codes = append(codes, in.TsCode)
		byCode[in.TsCode] = in
	}
	pts, err := repo.BarsForCodes(ctx, codes, dates)
	if err != nil {
		return rep, err
	}
	series := make(map[string][]store.ClosePoint, len(codes))
	for _, p := range pts {
		series[p.TsCode] = append(series[p.TsCode], p)
	}
	lastDate := dates[len(dates)-1]

	tr := &etfTracer{rep: rep}
	// ① 资格级：白名单条目自身完整（有人工复核过的名称）+ 当日有报价 + 窗口内日线齐全。
	//
	// 判定次序是"缺当日报价"在前、"窗口根数不足"在后，两者是可区分的故障：
	// 新入库的 ETF 一定缺根数而一定带着当日报价，缺当日报价则是 fund_daily 那天
	// 没同步成功。反过来排会让所有同步故障都归因成"历史不足"，把数据链问题
	// 说成"这只标的还不够资格"。
	live := make([]ETFInstrument, 0, len(opts.Pool))
	for _, in := range opts.Pool {
		ss := series[in.TsCode]
		switch {
		case !etfCodeOK(in.TsCode):
			// 最后一道：白名单是配置项，装配期校验之外还得防"旧二进制 + 手工改库"
			// 这类绕过 validate 的路径。个股误写进来时这里点名剔掉，不让它披 ETF 的身份进 LLM。
			tr.drop(etfSlugPool, reasonEtfBadCode)
		case in.Name == "":
			tr.drop(etfSlugPool, reasonEtfNoName)
		case len(ss) == 0:
			tr.drop(etfSlugPool, fmt.Sprintf("%s：窗口 %d 根只有 0 根", reasonEtfNoBar, window))
		case ss[len(ss)-1].TradeDate != lastDate:
			tr.drop(etfSlugPool, reasonEtfStale)
		case len(ss) < window:
			tr.drop(etfSlugPool, fmt.Sprintf("%s：窗口 %d 根只有 %d 根", reasonEtfNoBar, window, len(ss)))
		default:
			live = append(live, in)
		}
	}
	tr.emit(etfSlugPool, "ETF白名单(代码段+名称+日线完整)", len(opts.Pool), len(live))

	// ② 流动性级：窗口日均成交额达标 + 一手成本落在预算线内。
	//
	// 成交额按 未复权收盘(分/股) × 成交量(手) 近似（数值上即元），与 LLM 证据里的
	// AvgAmtYuan 同一算法：daily_bar 不存 amount 字段，而这一级的判定线是"亿元"量级，
	// VWAP 与收盘价的 1~3% 偏差不会让结论翻面。
	// 一手成本必须用未复权价（与成交成本同口径）：前复权价算出来的"一手 91 元"
	// 在某次份额折算后就是假的。
	var ok []ETFInstrument
	for _, in := range live {
		ss := series[in.TsCode]
		last := ss[len(ss)-1]
		var amt float64
		for _, p := range ss {
			amt += p.RawClose * p.VolLot * 100 // 分
		}
		avgFen := model.Fen(amt / float64(len(ss)))
		if opts.MinAvgAmountFen > 0 && avgFen < opts.MinAvgAmountFen {
			tr.drop(etfSlugLiq, fmt.Sprintf("%s：窗口日均 %.2f 亿 < %.2f 亿",
				reasonEtfIlliquid, avgFen.Float()/1e8, opts.MinAvgAmountFen.Float()/1e8))
			continue
		}
		// 一手成本交给 affordableStage 统一乘一手股数：与个股那一级共用同一条预算线，
		// 不另造一份"ETF 特殊口径"。
		if pass, why := affordableStage(model.Fen(math.Round(last.RawClose)), budget.perSlot()); !pass {
			tr.drop(etfSlugLiq, why)
			continue
		}
		ok = append(ok, in)
	}
	tr.emit(etfSlugLiq, "ETF流动性(日均成交额/一手预算)", len(live), len(ok))

	// 产出候选：不排名、不打分，Score/Factors/PoolSize 一律留零值，
	// 因为"这只 ETF 排第 1"这个信息在这批标的上不存在（见文件头 RankIC 实测）。
	for i, in := range ok {
		ss := series[in.TsCode]
		first, last := ss[0], ss[len(ss)-1]
		mom := 0.0
		if first.Close > 0 {
			mom = last.Close/first.Close - 1
		}
		rep.Candidates = append(rep.Candidates, model.Candidate{
			Rank:     i + 1,
			Kind:     model.KindETF,
			TsCode:   in.TsCode,
			Name:     in.Name,
			Industry: in.Track,
			Close:    model.Fen(math.Round(last.RawClose)),
			Mom:      mom,
			Reason:   fmt.Sprintf("ETF 白名单直通（窗口 %d 日，不排名）", window),
		})
	}
	return rep, nil
}

// etfTracer 两级漏斗的计数采集（与个股漏斗的 tracer 同职责，但不共用类型：
// 那个的 emit 签名绑死了 []model.StockBasic，白名单条目不是股票）。
type etfTracer struct {
	rep   *ETFReport
	drops map[string]map[string]int
}

func (t *etfTracer) drop(slug, why string) {
	if t.drops == nil {
		t.drops = map[string]map[string]int{}
	}
	if t.drops[slug] == nil {
		t.drops[slug] = map[string]int{}
	}
	t.drops[slug][why]++
}

func (t *etfTracer) emit(slug, name string, in, out int) {
	t.rep.Stages = append(t.rep.Stages, ETFStage{
		Stage: len(t.rep.Stages) + 1, Slug: slug, Name: name, In: in, Out: out, Drops: t.drops[slug],
	})
	delete(t.drops, slug)
}
