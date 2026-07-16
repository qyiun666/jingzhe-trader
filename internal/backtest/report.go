package backtest

import (
	"fmt"
	"os"
	"path/filepath"

	"jingzhe-trader/internal/model"
)

// GenerateHTMLReport 生成HTML回测报告
func GenerateHTMLReport(result *BacktestResult, outputPath string) error {
	dir := filepath.Dir(outputPath)
	if dir != "" && dir != "." {
		os.MkdirAll(dir, 0755)
	}

	f, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建报告文件失败: %w", err)
	}
	defer f.Close()

	m := result.Metrics

	// 生成净值曲线数据
	var navData string
	for i, snap := range result.Snapshots {
		if i > 0 {
			navData += ","
		}
		date := formatDisplayDate(snap.TradeDate)
		navData += fmt.Sprintf(`["%s",%.2f,%.4f]`, date, snap.TotalAsset, snap.TotalPnLPct)
	}

	// 生成交易明细
	var tradeRows string
	for _, t := range result.Trades {
		side := "买入"
		if t.Side == model.SideSell {
			side = "卖出"
		}
		tradeRows += fmt.Sprintf(
			`<tr><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%.2f</td><td>%.2f</td><td>%.2f</td></tr>`,
			formatDisplayDate(t.TradeDate), t.TsCode, side, t.Qty, t.Price, t.Amount, t.TotalCost,
		)
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>回测报告 - %s</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f5f5f5; color: #333; }
.container { max-width: 1200px; margin: 0 auto; padding: 20px; }
h1 { color: #1a1a2e; margin-bottom: 20px; }
.summary { display: grid; grid-template-columns: repeat(auto-fill, minmax(200px, 1fr)); gap: 15px; margin-bottom: 30px; }
.card { background: #fff; border-radius: 8px; padding: 20px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
.card .label { font-size: 12px; color: #888; margin-bottom: 5px; }
.card .value { font-size: 24px; font-weight: 600; }
.positive { color: #e74c3c; }
.negative { color: #27ae60; }
.section { background: #fff; border-radius: 8px; padding: 20px; margin-bottom: 20px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
.section h2 { font-size: 18px; margin-bottom: 15px; color: #1a1a2e; }
table { width: 100%%; border-collapse: collapse; font-size: 13px; }
th, td { padding: 8px 12px; text-align: left; border-bottom: 1px solid #eee; }
th { background: #f8f8f8; font-weight: 600; color: #555; }
.buy { color: #e74c3c; }
.sell { color: #27ae60; }
#chart { width: 100%%; height: 400px; margin-top: 10px; }
</style>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
</head>
<body>
<div class="container">
<h1>回测报告</h1>

<div class="section">
<h2>基本信息</h2>
<p>策略: <strong>%s</strong> | 回测区间: <strong>%s ~ %s</strong> | 运行ID: %s</p>
</div>

<div class="summary">
<div class="card"><div class="label">初始资金</div><div class="value">¥%.2f</div></div>
<div class="card"><div class="label">最终资产</div><div class="value">¥%.2f</div></div>
<div class="card"><div class="label">总收益率</div><div class="value %s">%.2f%%</div></div>
<div class="card"><div class="label">年化收益</div><div class="value %s">%.2f%%</div></div>
<div class="card"><div class="label">夏普比率</div><div class="value">%.2f</div></div>
<div class="card"><div class="label">最大回撤</div><div class="value negative">%.2f%%</div></div>
<div class="card"><div class="label">胜率</div><div class="value">%.2f%%</div></div>
<div class="card"><div class="label">盈亏比</div><div class="value">%.2f</div></div>
<div class="card"><div class="label">交易次数</div><div class="value">%d</div></div>
<div class="card"><div class="label">Alpha</div><div class="value">%.4f</div></div>
<div class="card"><div class="label">Beta</div><div class="value">%.4f</div></div>
</div>

<div class="section">
<h2>净值曲线</h2>
<canvas id="chart"></canvas>
</div>

<div class="section">
<h2>交易明细 (%d 笔)</h2>
<table>
<thead><tr><th>日期</th><th>代码</th><th>方向</th><th>数量</th><th>价格</th><th>金额</th><th>费用</th></tr></thead>
<tbody>
%s
</tbody>
</table>
</div>

</div>

<script>
const navData = [%s];
const ctx = document.getElementById('chart').getContext('2d');
new Chart(ctx, {
  type: 'line',
  data: {
    labels: navData.map(d => d[0]),
    datasets: [{
      label: '总资产',
      data: navData.map(d => d[1]),
      borderColor: '#3498db',
      backgroundColor: 'rgba(52,152,219,0.1)',
      fill: true,
      tension: 0.1,
      yAxisID: 'y'
    },{
      label: '累计收益率(%%)',
      data: navData.map(d => d[2]*100),
      borderColor: '#e74c3c',
      backgroundColor: 'rgba(231,76,60,0.1)',
      fill: false,
      tension: 0.1,
      yAxisID: 'y1'
    }]
  },
  options: {
    responsive: true,
    scales: {
      y: { type: 'linear', position: 'left', title: { text: '总资产(¥)', display: true } },
      y1: { type: 'linear', position: 'right', title: { text: '收益率(%%)', display: true }, grid: { drawOnChartArea: false } }
    }
  }
});
</script>
</body>
</html>`,
		result.StrategyName,
		result.StrategyName, result.StartDate, result.EndDate, result.RunID,
		result.InitialCapital,
		result.Snapshots[len(result.Snapshots)-1].TotalAsset,
		returnClass(m.TotalReturn), m.TotalReturn*100,
		returnClass(m.AnnualReturn), m.AnnualReturn*100,
		m.SharpeRatio,
		m.MaxDrawdown*100,
		m.WinRate*100,
		m.ProfitLossRatio,
		m.TotalTrades,
		m.Alpha, m.Beta,
		len(result.Trades),
		tradeRows,
		navData,
	)

	_, err = f.WriteString(html)
	return err
}

func returnClass(v float64) string {
	if v >= 0 {
		return "positive"
	}
	return "negative"
}

func formatDisplayDate(dateStr string) string {
	if len(dateStr) != 8 {
		return dateStr
	}
	return dateStr[:4] + "-" + dateStr[4:6] + "-" + dateStr[6:8]
}
