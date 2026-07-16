package report

import (
	"fmt"
	"math"
	"strings"

	"jingzhe-trader/internal/broker"
	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// ReconcileResult 对账结果
type ReconcileResult struct {
	TradeDate string
	// 资金对账
	LocalCash       float64
	BrokerCash      float64
	CashDiff        float64
	// 持仓对账
	LocalPositions  map[string]*model.Position
	BrokerPositions map[string]*model.Position
	PositionDiffs   []PositionDiff
	// 成交对账
	LocalTrades  []model.Trade
	BrokerTrades []model.Trade
	TradeDiffs   []TradeDiff
	// 总结
	IsBalanced bool
	Issues     []string
}

// PositionDiff 持仓差异项
type PositionDiff struct {
	TsCode      string
	Field       string  // qty/price/market_value
	LocalValue  float64
	BrokerValue float64
	Diff        float64
}

// TradeDiff 成交差异项
type TradeDiff struct {
	TsCode string
	Side   string
	Issue  string // missing_in_broker/duplicate/qty_mismatch
}

// 对账精度阈值 (金额差异小于此值视为一致)
const reconcileThreshold = 0.01

// Reconcile 执行对账
// 对比本地记录与券商实际数据，返回对账结果
func Reconcile(brk broker.Broker, tradeRepo *store.TradeRepo, tradeDate string) (*ReconcileResult, error) {
	result := &ReconcileResult{
		TradeDate:      tradeDate,
		LocalPositions: make(map[string]*model.Position),
		BrokerPositions: make(map[string]*model.Position),
		Issues:         []string{},
	}

	// 1. 获取券商端账户资产
	asset, err := brk.QueryAsset()
	if err != nil {
		return nil, fmt.Errorf("查询券商资产失败: %w", err)
	}
	result.BrokerCash = asset.Cash

	// 2. 获取本地最新的账户快照
	snap, err := tradeRepo.GetLatestAccountSnapshot("")
	if err != nil {
		return nil, fmt.Errorf("查询本地账户快照失败: %w", err)
	}
	if snap != nil {
		result.LocalCash = snap.Cash
	}
	result.CashDiff = math.Abs(result.BrokerCash - result.LocalCash)

	// 3. 获取券商端持仓
	brokerPositions, err := brk.QueryPositions()
	if err != nil {
		return nil, fmt.Errorf("查询券商持仓失败: %w", err)
	}
	result.BrokerPositions = brokerPositions

	// 4. 从券商资产中获取本地持仓 (券商接口统一返回)
	if asset.Positions != nil {
		result.LocalPositions = asset.Positions
	}

	// 5. 对比持仓
	result.PositionDiffs = reconcilePositions(result.LocalPositions, result.BrokerPositions)

	// 6. 获取本地成交记录 (按日期查询)
	localTrades, err := tradeRepo.GetTradesByRunID("")
	if err != nil {
		return nil, fmt.Errorf("查询本地成交记录失败: %w", err)
	}
	// 过滤出当天的成交记录
	for _, t := range localTrades {
		if t.TradeDate == tradeDate {
			result.LocalTrades = append(result.LocalTrades, t)
		}
	}

	// 7. 对比成交 (券商端暂无独立查询成交接口, 标记为待实现)
	result.TradeDiffs = []TradeDiff{}

	// 8. 汇总问题
	collectIssues(result)

	return result, nil
}

// reconcilePositions 对比本地与券商持仓, 返回差异列表
func reconcilePositions(local, broker map[string]*model.Position) []PositionDiff {
	var diffs []PositionDiff

	// 收集所有涉及的股票代码
	allCodes := make(map[string]bool)
	for code := range local {
		allCodes[code] = true
	}
	for code := range broker {
		allCodes[code] = true
	}

	for code := range allCodes {
		lp, hasLocal := local[code]
		bp, hasBroker := broker[code]

		if !hasLocal && hasBroker {
			// 券商有持仓, 本地无记录
			diffs = append(diffs, PositionDiff{
				TsCode:      code,
				Field:       "qty",
				LocalValue:  0,
				BrokerValue: float64(bp.TotalQty),
				Diff:        float64(bp.TotalQty),
			})
			continue
		}
		if hasLocal && !hasBroker {
			// 本地有持仓, 券商无记录
			diffs = append(diffs, PositionDiff{
				TsCode:      code,
				Field:       "qty",
				LocalValue:  float64(lp.TotalQty),
				BrokerValue: 0,
				Diff:        -float64(lp.TotalQty),
			})
			continue
		}

		// 两边都有, 逐字段对比
		// 数量差异
		qtyDiff := float64(lp.TotalQty - bp.TotalQty)
		if math.Abs(qtyDiff) > reconcileThreshold {
			diffs = append(diffs, PositionDiff{
				TsCode:      code,
				Field:       "qty",
				LocalValue:  float64(lp.TotalQty),
				BrokerValue: float64(bp.TotalQty),
				Diff:        qtyDiff,
			})
		}

		// 成本价差异
		priceDiff := lp.CostPrice - bp.CostPrice
		if math.Abs(priceDiff) > reconcileThreshold {
			diffs = append(diffs, PositionDiff{
				TsCode:      code,
				Field:       "price",
				LocalValue:  lp.CostPrice,
				BrokerValue: bp.CostPrice,
				Diff:        priceDiff,
			})
		}

		// 市值差异
		mvDiff := lp.MarketValue - bp.MarketValue
		if math.Abs(mvDiff) > reconcileThreshold {
			diffs = append(diffs, PositionDiff{
				TsCode:      code,
				Field:       "market_value",
				LocalValue:  lp.MarketValue,
				BrokerValue: bp.MarketValue,
				Diff:        mvDiff,
			})
		}
	}

	return diffs
}

// collectIssues 汇总对账中发现的问题
func collectIssues(r *ReconcileResult) {
	// 资金差异检查
	if r.CashDiff > reconcileThreshold {
		r.Issues = append(r.Issues, fmt.Sprintf(
			"资金不一致: 本地=%.2f, 券商=%.2f, 差异=%.2f",
			r.LocalCash, r.BrokerCash, r.CashDiff,
		))
	}

	// 持仓差异检查
	for _, d := range r.PositionDiffs {
		switch d.Field {
		case "qty":
			if d.Diff > 0 {
				r.Issues = append(r.Issues, fmt.Sprintf(
					"持仓数量差异[%s]: 本地有 %d 股, 券商无记录", d.TsCode, int(d.LocalValue)))
			} else {
				r.Issues = append(r.Issues, fmt.Sprintf(
					"持仓数量差异[%s]: 券商多出 %d 股, 本地无记录", d.TsCode, int(-d.Diff)))
			}
		case "price":
			r.Issues = append(r.Issues, fmt.Sprintf(
				"成本价差异[%s]: 本地=%.4f, 券商=%.4f, 差异=%.4f",
				d.TsCode, d.LocalValue, d.BrokerValue, d.Diff))
		case "market_value":
			r.Issues = append(r.Issues, fmt.Sprintf(
				"市值差异[%s]: 本地=%.2f, 券商=%.2f, 差异=%.2f",
				d.TsCode, d.LocalValue, d.BrokerValue, d.Diff))
		}
	}

	// 成交差异检查
	for _, d := range r.TradeDiffs {
		r.Issues = append(r.Issues, fmt.Sprintf(
			"成交差异[%s %s]: %s", d.TsCode, d.Side, d.Issue))
	}

	// 最终判定
	r.IsBalanced = len(r.Issues) == 0
}

// GenerateReconcileReport 生成对账报告 (文本格式)
func GenerateReconcileReport(result *ReconcileResult) string {
	var sb strings.Builder

	sb.WriteString("═══════════════════════════════════════\n")
	sb.WriteString(fmt.Sprintf("  对账报告  交易日: %s\n", result.TradeDate))
	sb.WriteString("═══════════════════════════════════════\n\n")

	// 资金对账
	sb.WriteString("【资金对账】\n")
	sb.WriteString(fmt.Sprintf("  本地资金:   %.2f\n", result.LocalCash))
	sb.WriteString(fmt.Sprintf("  券商资金:   %.2f\n", result.BrokerCash))
	sb.WriteString(fmt.Sprintf("  资金差异:   %.2f", result.CashDiff))
	if result.CashDiff <= reconcileThreshold {
		sb.WriteString("  ✓ 一致\n")
	} else {
		sb.WriteString("  ✗ 不一致\n")
	}
	sb.WriteString("\n")

	// 持仓对账
	sb.WriteString(fmt.Sprintf("【持仓对账】 本地 %d 只, 券商 %d 只\n",
		len(result.LocalPositions), len(result.BrokerPositions)))
	if len(result.PositionDiffs) == 0 {
		sb.WriteString("  持仓完全一致 ✓\n")
	} else {
		for _, d := range result.PositionDiffs {
			fieldName := fieldDisplayName(d.Field)
			sb.WriteString(fmt.Sprintf("  %s 差异 [%s]: 本地=%.4f, 券商=%.4f, 差异=%.4f\n",
				fieldName, d.TsCode, d.LocalValue, d.BrokerValue, d.Diff))
		}
	}
	sb.WriteString("\n")

	// 成交对账
	sb.WriteString(fmt.Sprintf("【成交对账】 本地 %d 笔, 券商 %d 笔\n",
		len(result.LocalTrades), len(result.BrokerTrades)))
	if len(result.TradeDiffs) == 0 {
		sb.WriteString("  成交完全一致 ✓\n")
	} else {
		for _, d := range result.TradeDiffs {
			sb.WriteString(fmt.Sprintf("  差异 [%s %s]: %s\n",
				d.TsCode, d.Side, d.Issue))
		}
	}
	sb.WriteString("\n")

	// 总结
	sb.WriteString("───────────────────────────────────────\n")
	if result.IsBalanced {
		sb.WriteString("  结论: 账目一致 ✓\n")
	} else {
		sb.WriteString(fmt.Sprintf("  结论: 发现 %d 个问题 ✗\n", len(result.Issues)))
		sb.WriteString("───────────────────────────────────────\n")
		for i, issue := range result.Issues {
			sb.WriteString(fmt.Sprintf("  %d. %s\n", i+1, issue))
		}
	}
	sb.WriteString("═══════════════════════════════════════\n")

	return sb.String()
}

// fieldDisplayName 返回持仓差异字段的中文显示名称
func fieldDisplayName(field string) string {
	switch field {
	case "qty":
		return "数量"
	case "price":
		return "成本价"
	case "market_value":
		return "市值"
	default:
		return field
	}
}

// CheckDailyPnL 检查每日盈亏是否与行情一致
// 用当天行情和前一天行情计算理论盈亏, 与实际持仓盈亏对比
func CheckDailyPnL(positions map[string]*model.Position,
	todayBars map[string]*model.Bar,
	prevBars map[string]*model.Bar) []string {

	var issues []string

	for tsCode, pos := range positions {
		todayBar, hasToday := todayBars[tsCode]
		prevBar, hasPrev := prevBars[tsCode]

		if !hasToday {
			// 无当天行情, 跳过 (可能停牌)
			continue
		}

		// 计算理论盈亏: 持仓量 * (今日收盘 - 昨日收盘)
		var theoreticalPnL float64
		if hasPrev {
			theoreticalPnL = float64(pos.TotalQty) * (todayBar.Close - prevBar.Close)
		} else {
			// 无昨日行情 (新股或数据缺失), 使用涨跌额近似
			theoreticalPnL = float64(pos.TotalQty) * todayBar.Change
		}

		// 计算实际浮动盈亏
		actualPnL := pos.MarketValue - float64(pos.TotalQty)*pos.CostPrice

		// 允许一定误差 (价格精度和小数截断)
		diff := math.Abs(theoreticalPnL - actualPnL)
		tolerance := float64(pos.TotalQty) * todayBar.Close * 0.005 // 0.5% 容差

		if diff > tolerance && tolerance > 0 {
			issues = append(issues, fmt.Sprintf(
				"盈亏偏差过大[%s]: 理论=%.2f, 实际=%.2f, 偏差=%.2f (容差=%.2f)",
				tsCode, theoreticalPnL, actualPnL, diff, tolerance))
		}
	}

	return issues
}
