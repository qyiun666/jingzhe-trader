package report

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"jingzhe-trader/internal/analysis"
	"jingzhe-trader/internal/model"
)

// DailyReport 每日操盘报告
type DailyReport struct {
	Date              string
	MarketSnapshot    *analysis.MarketSnapshot
	PortfolioAnalysis *PortfolioSummary
	RebalancePlan     *RebalanceSummary
	StrategyAdvice    *StrategySummary
	NewsSummary       *NewsSummary
	ActionItems       []ActionItem
}

// ActionItem 执行清单项
type ActionItem struct {
	Time     string // "09:25" / "09:30" / "14:50" / "盘后"
	Action   string // "卖出" / "买入" / "观望" / "检查"
	TsCode   string
	Name     string
	Detail   string
	Priority int
}

// GenerateDailyReport 生成每日操盘报告 HTML 文件
func GenerateDailyReport(report *DailyReport, outputPath string) error {
	dir := filepath.Dir(outputPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建报告目录失败: %w", err)
		}
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建报告文件失败: %w", err)
	}
	defer f.Close()

	html := buildHTML(report)
	_, err = f.WriteString(html)
	return err
}

// buildHTML 拼接完整 HTML
func buildHTML(r *DailyReport) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>每日操盘报告 - %s</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif; background: #f0f2f5; color: #333; line-height: 1.6; }
.container { max-width: 1280px; margin: 0 auto; padding: 24px; }
header { background: linear-gradient(135deg, #1a1a2e 0%%, #16213e 100%%); color: #fff; padding: 30px; border-radius: 12px; margin-bottom: 24px; box-shadow: 0 4px 12px rgba(0,0,0,0.15); }
header h1 { font-size: 28px; font-weight: 600; margin-bottom: 8px; }
header .subtitle { font-size: 14px; opacity: 0.85; }
.section { background: #fff; border-radius: 12px; padding: 24px; margin-bottom: 20px; box-shadow: 0 2px 8px rgba(0,0,0,0.06); }
.section h2 { font-size: 18px; font-weight: 600; color: #1a1a2e; margin-bottom: 16px; padding-bottom: 10px; border-bottom: 2px solid #e8e8e8; display: flex; align-items: center; }
.section h2 .icon { width: 6px; height: 18px; background: #3498db; border-radius: 3px; margin-right: 10px; display: inline-block; }
.grid-4 { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 16px; }
.grid-3 { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 16px; }
.card { background: #fafbfc; border-radius: 10px; padding: 18px; border: 1px solid #eaecef; }
.card .label { font-size: 12px; color: #888; margin-bottom: 6px; text-transform: uppercase; letter-spacing: 0.5px; }
.card .value { font-size: 22px; font-weight: 700; color: #1a1a2e; }
.card .sub { font-size: 12px; color: #999; margin-top: 4px; }
.positive { color: #e74c3c; }
.negative { color: #27ae60; }
.neutral { color: #f39c12; }
table { width: 100%%; border-collapse: collapse; font-size: 13px; margin-top: 8px; }
th, td { padding: 10px 12px; text-align: left; border-bottom: 1px solid #eee; }
th { background: #f8f9fa; font-weight: 600; color: #555; font-size: 12px; text-transform: uppercase; }
tr:hover { background: #fafbfc; }
.badge { display: inline-block; padding: 3px 10px; border-radius: 20px; font-size: 11px; font-weight: 600; }
.badge-red { background: #fdeaea; color: #c0392b; }
.badge-orange { background: #fef3e2; color: #d35400; }
.badge-green { background: #e8f8f0; color: #27ae60; }
.badge-gray { background: #f0f0f0; color: #7f8c8d; }
.badge-blue { background: #e3f2fd; color: #1976d2; }
.alarm-danger { background: #fdeaea; border-left: 4px solid #e74c3c; padding: 12px 16px; border-radius: 6px; margin-bottom: 8px; }
.alarm-warning { background: #fef3e2; border-left: 4px solid #f39c12; padding: 12px 16px; border-radius: 6px; margin-bottom: 8px; }
.alarm-info { background: #e3f2fd; border-left: 4px solid #3498db; padding: 12px 16px; border-radius: 6px; margin-bottom: 8px; }
.alarm-level { font-size: 11px; font-weight: 700; text-transform: uppercase; margin-bottom: 4px; }
.alarm-danger .alarm-level { color: #c0392b; }
.alarm-warning .alarm-level { color: #d35400; }
.alarm-info .alarm-level { color: #1976d2; }
.timeline { position: relative; padding-left: 24px; }
.timeline::before { content: ''; position: absolute; left: 6px; top: 0; bottom: 0; width: 2px; background: #e0e0e0; }
.timeline-item { position: relative; margin-bottom: 18px; }
.timeline-item::before { content: ''; position: absolute; left: -22px; top: 4px; width: 10px; height: 10px; border-radius: 50%%; background: #3498db; border: 2px solid #fff; box-shadow: 0 0 0 2px #3498db; }
.timeline-time { font-size: 13px; font-weight: 700; color: #555; margin-bottom: 4px; }
.timeline-content { background: #fafbfc; padding: 12px 16px; border-radius: 8px; border: 1px solid #eaecef; }
.timeline-action { font-weight: 600; color: #1a1a2e; margin-bottom: 4px; }
.health-score { display: flex; align-items: center; gap: 16px; }
.health-circle { width: 80px; height: 80px; border-radius: 50%%; background: conic-gradient(#27ae60 %d%%, #eee 0); display: flex; align-items: center; justify-content: center; font-size: 24px; font-weight: 700; color: #1a1a2e; position: relative; }
.health-circle::before { content: ''; position: absolute; width: 64px; height: 64px; background: #fff; border-radius: 50%%; }
.health-circle span { position: relative; z-index: 1; }
.sector-bar { display: flex; align-items: center; margin-bottom: 8px; }
.sector-name { width: 100px; font-size: 13px; color: #555; }
.sector-track { flex: 1; height: 8px; background: #eee; border-radius: 4px; overflow: hidden; margin: 0 10px; }
.sector-fill { height: 100%%; background: #3498db; border-radius: 4px; }
.sector-pct { width: 50px; font-size: 12px; color: #888; text-align: right; }
.footer { text-align: center; padding: 20px; color: #999; font-size: 12px; }
.summary-row { display: flex; gap: 20px; flex-wrap: wrap; margin-bottom: 12px; }
.summary-item { flex: 1; min-width: 140px; }
</style>
</head>
<body>
<div class="container">
%s
%s
%s
%s
%s
%s
%s
%s
<div class="footer">报告生成时间: %s | chaogu 操盘助手</div>
</div>
</body>
</html>`,
		r.Date,
		healthScorePercent(r.PortfolioAnalysis),
		buildHeader(r),
		buildMarketOverview(r.MarketSnapshot),
		buildPortfolioDiagnosis(r.PortfolioAnalysis),
		buildNewsSection(r.NewsSummary),
		buildStrategyAdvice(r.StrategyAdvice),
		buildActionTimeline(r.ActionItems),
		buildTradeList(r),
		buildTomorrowPlan(r),
		nowTime(),
	)
}

func healthScorePercent(pa *PortfolioSummary) int {
	if pa == nil {
		return 0
	}
	return pa.HealthScore
}

func nowTime() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func buildHeader(r *DailyReport) string {
	var alarmsHTML string
	if r.MarketSnapshot != nil && len(r.MarketSnapshot.Alarms) > 0 {
		alarmsHTML = `<div style="margin-top:16px;">`
		for _, a := range r.MarketSnapshot.Alarms {
			cls := "alarm-info"
			if a.Level == "danger" {
				cls = "alarm-danger"
			} else if a.Level == "warning" {
				cls = "alarm-warning"
			}
			alarmsHTML += fmt.Sprintf(`<div class="%s"><div class="alarm-level">%s</div><div>%s</div></div>`, cls, a.Level, a.Message)
		}
		alarmsHTML += `</div>`
	}

	strategyName := "未指定"
	if r.StrategyAdvice != nil {
		strategyName = r.StrategyAdvice.StrategyName
	}

	return fmt.Sprintf(`<header>
<h1>每日操盘报告</h1>
<div class="subtitle">日期: %s | 策略: %s | 报告类型: 每日操盘</div>
%s
</header>`, r.Date, strategyName, alarmsHTML)
}

func buildMarketOverview(ms *analysis.MarketSnapshot) string {
	if ms == nil {
		return `<div class="section"><h2><span class="icon"></span>市场概况</h2><p>暂无市场数据</p></div>`
	}

	// 指数卡片
	var indexCards string
	indexOrder := []string{"000001.SH", "399001.SZ", "399006.SZ", "000300.SH", "000905.SH", "000688.SH"}
	indexNames := map[string]string{
		"000001.SH": "上证指数", "399001.SZ": "深证成指", "399006.SZ": "创业板指",
		"000300.SH": "沪深300", "000905.SH": "中证500", "000688.SH": "科创50",
	}
	for _, code := range indexOrder {
		bar, ok := ms.IndexBars[code]
		if !ok {
			continue
		}
		cls := "positive"
		if bar.PctChg < 0 {
			cls = "negative"
		}
		indexCards += fmt.Sprintf(
			`<div class="card"><div class="label">%s</div><div class="value %s">%.2f</div><div class="sub">%.2f%%</div></div>`,
			indexNames[code], cls, bar.Close, bar.PctChg,
		)
	}

	// 涨跌统计
	upDownColor := "positive"
	if ms.UpCount < ms.DownCount {
		upDownColor = "negative"
	}

	// 热点板块
	var sectorRows string
	for _, s := range ms.HotSectors {
		cls := "positive"
		if s.AvgChange < 0 {
			cls = "negative"
		}
		sectorRows += fmt.Sprintf(`<tr><td>%s</td><td class="%s">%+.2f%%</td><td>%s</td><td class="%s">%+.2f%%</td></tr>`,
			s.Sector, cls, s.AvgChange, s.LeaderStock, cls, s.LeaderChange)
	}
	sectorTable := ""
	if sectorRows != "" {
		sectorTable = fmt.Sprintf(`<h3 style="margin:16px 0 8px;font-size:14px;color:#555;">热点板块 TOP3</h3>
<table><thead><tr><th>板块</th><th>平均涨跌</th><th>领涨股</th><th>领涨股涨跌</th></tr></thead><tbody>%s</tbody></table>`, sectorRows)
	}

	vrStatus := "正常"
	if ms.VolumeRatio > 1.5 {
		vrStatus = "放量"
	} else if ms.VolumeRatio < 0.8 {
		vrStatus = "缩量"
	}

	return fmt.Sprintf(`<div class="section">
<h2><span class="icon"></span>市场概况</h2>
<div class="grid-4">%s</div>
<div style="margin-top:20px;" class="grid-4">
<div class="card"><div class="label">上涨家数</div><div class="value positive">%d</div></div>
<div class="card"><div class="label">下跌家数</div><div class="value negative">%d</div></div>
<div class="card"><div class="label">涨停 / 跌停</div><div class="value %s">%d / %d</div></div>
<div class="card"><div class="label">全市场量比</div><div class="value">%.2f</div><div class="sub">%s</div></div>
</div>
%s
</div>`, indexCards, ms.UpCount, ms.DownCount, upDownColor, ms.UpLimitCount, ms.DownLimitCount, ms.VolumeRatio, vrStatus, sectorTable)
}

func buildPortfolioDiagnosis(pa *PortfolioSummary) string {
	if pa == nil {
		return `<div class="section"><h2><span class="icon"></span>持仓诊断</h2><p>暂无持仓数据</p></div>`
	}

	score := pa.HealthScore
	scoreColor := "#27ae60"
	if score < 60 {
		scoreColor = "#e74c3c"
	} else if score < 80 {
		scoreColor = "#f39c12"
	}

	// 板块分布
	var sectorHTML string
	if len(pa.SectorDist) > 0 {
		var sectors []struct {
			Name string
			Pct  float64
		}
		for k, v := range pa.SectorDist {
			sectors = append(sectors, struct{ Name string; Pct float64 }{k, v})
		}
		sort.Slice(sectors, func(i, j int) bool { return sectors[i].Pct > sectors[j].Pct })
		var maxPct float64
		for _, s := range sectors {
			if s.Pct > maxPct {
				maxPct = s.Pct
			}
		}
		for _, s := range sectors {
			w := 0.0
			if maxPct > 0 {
				w = s.Pct / maxPct * 100
			}
			sectorHTML += fmt.Sprintf(`<div class="sector-bar"><div class="sector-name">%s</div><div class="sector-track"><div class="sector-fill" style="width:%.1f%%;"></div></div><div class="sector-pct">%.1f%%</div></div>`, s.Name, w, s.Pct*100)
		}
	}

	// 持仓明细
	var posRows string
	for _, p := range pa.Positions {
		cls := "positive"
		if p.FloatingPnL < 0 {
			cls = "negative"
		}
		posRows += fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td>%d</td><td>%.2f</td><td>%.2f</td><td class="%s">%.2f</td><td class="%s">%.2f%%</td><td>%.1f%%</td><td><span class="badge badge-%s">%s</span></td></tr>`,
			p.TsCode, p.Name, p.TotalQty, p.CostPrice, p.MarketPrice, cls, p.FloatingPnL, cls, p.FloatingPnLPct*100, p.WeightPct*100, riskBadgeColor(p.RiskLevel), p.RiskLevel)
	}

	return fmt.Sprintf(`<div class="section">
<h2><span class="icon"></span>持仓诊断</h2>
<div class="grid-3">
<div class="card">
<div class="label">总资产</div><div class="value">¥%.2f</div>
<div class="sub">市值 ¥%.2f + 现金 ¥%.2f</div>
</div>
<div class="card">
<div class="label">持仓集中度</div><div class="value">%.1f%%</div>
<div class="sub">最大持仓占比</div>
</div>
<div class="card">
<div class="label">健康度评分</div>
<div class="health-score"><div class="health-circle" style="background: conic-gradient(%s %d%%, #eee 0);"><span>%d</span></div></div>
</div>
</div>
<div style="margin-top:20px;">
<h3 style="margin-bottom:10px;font-size:14px;color:#555;">板块分布</h3>
%s
</div>
<div style="margin-top:20px;">
<h3 style="margin-bottom:10px;font-size:14px;color:#555;">持仓明细</h3>
<table>
<thead><tr><th>代码</th><th>名称</th><th>数量</th><th>成本</th><th>现价</th><th>浮动盈亏</th><th>盈亏率</th><th>权重</th><th>风险</th></tr></thead>
<tbody>%s</tbody>
</table>
</div>
</div>`, pa.TotalAsset, pa.MarketValue, pa.Cash, pa.Concentration*100, scoreColor, score, score, sectorHTML, posRows)
}

func riskBadgeColor(risk string) string {
	switch risk {
	case "high":
		return "red"
	case "medium":
		return "orange"
	default:
		return "green"
	}
}

func buildNewsSection(ns *NewsSummary) string {
	if ns == nil || len(ns.Items) == 0 {
		return fmt.Sprintf(`<div class="section"><h2><span class="icon"></span>新闻舆情</h2><p>暂无新闻数据 (市场情绪: <span class="badge badge-gray">%s</span>)</p></div>`, sentimentLabel(ns))
	}

	var items string
	for _, item := range ns.Items {
		badge := "gray"
		if item.Sentiment == "positive" {
			badge = "green"
		} else if item.Sentiment == "negative" {
			badge = "red"
		}
		items += fmt.Sprintf(`<div style="padding:10px 0;border-bottom:1px solid #f0f0f0;"><div style="font-weight:600;font-size:13px;">%s <span class="badge badge-%s">%s</span></div><div style="font-size:12px;color:#888;margin-top:4px;">%s | %s</div></div>`, item.Title, badge, item.Sentiment, item.Source, item.Time)
	}

	return fmt.Sprintf(`<div class="section">
<h2><span class="icon"></span>新闻舆情</h2>
<p style="margin-bottom:12px;">市场情绪: <span class="badge badge-%s">%s</span></p>
%s
</div>`, sentimentBadgeColor(ns.Sentiment), sentimentLabel(ns), items)
}

func sentimentLabel(ns *NewsSummary) string {
	if ns == nil {
		return "neutral"
	}
	return ns.Sentiment
}

func sentimentBadgeColor(s string) string {
	switch s {
	case "positive":
		return "green"
	case "negative":
		return "red"
	default:
		return "gray"
	}
}

func buildStrategyAdvice(sa *StrategySummary) string {
	if sa == nil {
		return `<div class="section"><h2><span class="icon"></span>策略建议</h2><p>暂无策略建议</p></div>`
	}

	actionBadge := "gray"
	switch sa.RecommendedAction {
	case "buy":
		actionBadge = "green"
	case "sell":
		actionBadge = "red"
	case "rebalance":
		actionBadge = "blue"
	}

	confidencePct := int(sa.Confidence * 100)

	return fmt.Sprintf(`<div class="section">
<h2><span class="icon"></span>策略建议</h2>
<div class="grid-3">
<div class="card"><div class="label">当前策略</div><div class="value">%s</div></div>
<div class="card"><div class="label">推荐操作</div><div class="value"><span class="badge badge-%s">%s</span></div></div>
<div class="card"><div class="label">信心度</div><div class="value">%d%%</div></div>
</div>
<div style="margin-top:16px;padding:16px;background:#fafbfc;border-radius:8px;border:1px solid #eaecef;">
<div style="font-weight:600;margin-bottom:8px;">策略理由</div>
<div style="font-size:13px;color:#555;line-height:1.8;">%s</div>
</div>
<div style="margin-top:12px;padding:12px 16px;background:#fff8e1;border-radius:8px;border-left:4px solid #f39c12;font-size:13px;color:#555;">
<strong>风险提示:</strong> %s
</div>
</div>`, sa.StrategyName, actionBadge, sa.RecommendedAction, confidencePct, sa.Reason, sa.RiskNote)
}

func buildActionTimeline(items []ActionItem) string {
	if len(items) == 0 {
		return `<div class="section"><h2><span class="icon"></span>今日操作清单</h2><p>暂无操作项</p></div>`
	}

	// 按时间分组
	timeOrder := []string{"09:25", "09:30", "盘中", "14:50", "盘后"}
	timeLabels := map[string]string{
		"09:25": "开盘前 (09:25)",
		"09:30": "开盘后 (09:30)",
		"盘中":  "盘中时段",
		"14:50": "尾盘 (14:50)",
		"盘后":  "盘后复盘",
	}

	grouped := make(map[string][]ActionItem)
	for _, it := range items {
		grouped[it.Time] = append(grouped[it.Time], it)
	}

	var timeline string
	for _, t := range timeOrder {
		group, ok := grouped[t]
		if !ok {
			continue
		}
		// 按优先级排序
		sort.Slice(group, func(i, j int) bool {
			return group[i].Priority < group[j].Priority
		})

		var content string
		for _, it := range group {
			actionColor := "#555"
			if it.Action == "卖出" {
				actionColor = "#e74c3c"
			} else if it.Action == "买入" {
				actionColor = "#27ae60"
			} else if it.Action == "检查" {
				actionColor = "#3498db"
			}
			nameStr := it.Name
			if nameStr == "" {
				nameStr = it.TsCode
			}
			content += fmt.Sprintf(`<div style="margin-bottom:8px;padding:8px;background:#fff;border-radius:6px;border:1px solid #eee;"><span style="color:%s;font-weight:600;">[%s]</span> %s <span style="color:#888;font-size:12px;">%s</span></div>`, actionColor, it.Action, nameStr, it.Detail)
		}

		timeline += fmt.Sprintf(`<div class="timeline-item"><div class="timeline-time">%s</div><div class="timeline-content">%s</div></div>`, timeLabels[t], content)
	}

	return fmt.Sprintf(`<div class="section">
<h2><span class="icon"></span>今日操作清单</h2>
<div class="timeline">%s</div>
</div>`, timeline)
}

func buildTradeList(r *DailyReport) string {
	if r.RebalancePlan == nil || len(r.RebalancePlan.Orders) == 0 {
		return `<div class="section"><h2><span class="icon"></span>买卖清单</h2><p>暂无调仓信号</p></div>`
	}

	// 分类信号
	var mustSell, canSell, mustBuy, watch []SignalSummary
	for _, sig := range r.RebalancePlan.Orders {
		switch {
		case sig.Direction == model.DirSell && sig.Strength > 0.7:
			mustSell = append(mustSell, sig)
		case sig.Direction == model.DirSell:
			canSell = append(canSell, sig)
		case sig.Direction == model.DirBuy:
			mustBuy = append(mustBuy, sig)
		default:
			watch = append(watch, sig)
		}
	}

	var sections string
	sections += buildSignalSection("必卖 (止损/风控)", "red", mustSell)
	sections += buildSignalSection("可卖 (止盈/调仓)", "orange", canSell)
	sections += buildSignalSection("必买 (策略信号)", "green", mustBuy)
	sections += buildSignalSection("观望", "gray", watch)

	return fmt.Sprintf(`<div class="section">
<h2><span class="icon"></span>买卖清单</h2>
%s
</div>`, sections)
}

func buildSignalSection(title, color string, signals []SignalSummary) string {
	if len(signals) == 0 {
		return ""
	}
	var rows string
	for _, s := range signals {
		rows += fmt.Sprintf(`<tr><td>%s</td><td>%d</td><td>%.0f%%</td><td>%s</td></tr>`, s.TsCode, s.TargetQty, s.Strength*100, s.Reason)
	}
	return fmt.Sprintf(`<h3 style="margin:16px 0 8px;font-size:14px;color:#555;"><span class="badge badge-%s">%s</span></h3>
<table><thead><tr><th>代码</th><th>目标数量</th><th>信号强度</th><th>理由</th></tr></thead><tbody>%s</tbody></table>`, color, title, rows)
}

func buildTomorrowPlan(r *DailyReport) string {
	var content string
	if r.StrategyAdvice != nil && r.StrategyAdvice.RecommendedAction != "" {
		content += fmt.Sprintf(`<div style="padding:12px 16px;background:#f0f7ff;border-radius:8px;margin-bottom:10px;border-left:4px solid #3498db;"><strong>策略预判:</strong> %s</div>`, r.StrategyAdvice.Reason)
	}
	if r.RebalancePlan != nil && r.RebalancePlan.Reason != "" {
		content += fmt.Sprintf(`<div style="padding:12px 16px;background:#f5fff5;border-radius:8px;margin-bottom:10px;border-left:4px solid #27ae60;"><strong>调仓预案:</strong> %s</div>`, r.RebalancePlan.Reason)
	}
	if content == "" {
		content = `<p style="color:#888;">暂无明确预案, 建议次日根据开盘情况灵活应对。</p>`
	}

	return fmt.Sprintf(`<div class="section">
<h2><span class="icon"></span>明日预案</h2>
%s
</div>`, content)
}
