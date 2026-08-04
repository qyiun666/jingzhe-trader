package agent

import (
	"fmt"
	"strings"

	"jingzhe-trader/internal/store"
	"jingzhe-trader/pkg/logger"
)

// DetectDecisionChanges 对比当日辩论结果与历史结果，检测决策变更
// 返回变更列表 (只包含发生变化的股票)
func (o *DebateOrchestrator) DetectDecisionChanges(todayResults []store.DebateResult) []DecisionChange {
	if o.debateRepo == nil || len(todayResults) == 0 {
		return nil
	}

	// 取当日最早一条记录的 trade_date 作为基准
	todayDate := todayResults[0].TradeDate
	prevMap, err := o.debateRepo.GetPreviousDebates(todayDate)
	if err != nil {
		logger.L().Warnw("获取历史辩论结果失败，跳过变更检测", "err", err)
		return nil
	}

	changes := make([]DecisionChange, 0)
	for _, curr := range todayResults {
		prev, exists := prevMap[curr.TsCode]
		if !exists {
			// 新增的辩论标的
			changes = append(changes, DecisionChange{
				TsCode:         curr.TsCode,
				Name:           curr.Name,
				PrevDecision:   "",
				CurrDecision:   curr.Decision,
				PrevConfidence: 0,
				CurrConfidence: curr.Confidence,
				Changed:        true,
				Detail:         fmt.Sprintf("新增标的: %s → %s (置信度 %.0f%%)", curr.Name, decisionLabel(curr.Decision), curr.Confidence*100),
			})
			continue
		}

		// 对比决策和置信度
		changed := false
		var details []string

		if prev.Decision != curr.Decision {
			changed = true
			details = append(details, fmt.Sprintf("决策变更: %s → %s",
				decisionLabel(prev.Decision), decisionLabel(curr.Decision)))
		}

		// 置信度变化超过 20% 视为显著变化
		confDelta := curr.Confidence - prev.Confidence
		if absFloat(confDelta) > 0.2 {
			changed = true
			direction := "上升"
			if confDelta < 0 {
				direction = "下降"
			}
			details = append(details, fmt.Sprintf("置信度%s: %.0f%% → %.0f%%",
				direction, prev.Confidence*100, curr.Confidence*100))
		}

		// 风险等级变化
		if prev.RiskLevel != curr.RiskLevel && prev.RiskLevel != "" && curr.RiskLevel != "" {
			changed = true
			details = append(details, fmt.Sprintf("风险等级: %s → %s", prev.RiskLevel, curr.RiskLevel))
		}

		if changed {
			changes = append(changes, DecisionChange{
				TsCode:         curr.TsCode,
				Name:           curr.Name,
				PrevDecision:   prev.Decision,
				CurrDecision:   curr.Decision,
				PrevConfidence: prev.Confidence,
				CurrConfidence: curr.Confidence,
				Changed:        true,
				Detail:         strings.Join(details, "; "),
			})
		}
	}

	if len(changes) > 0 {
		logger.L().Infof("[决策变更检测] 检测到 %d 个标的决策发生变化", len(changes))
	}
	return changes
}

// decisionLabel 决策中文标签
func decisionLabel(decision string) string {
	switch decision {
	case "buy":
		return "买入"
	case "hold":
		return "持有"
	case "reject":
		return "否决"
	case "sell":
		return "卖出"
	default:
		return decision
	}
}

// absFloat 浮点数绝对值
func absFloat(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// FormatChangesForNotify 格式化决策变更列表为通知文本
func FormatChangesForNotify(changes []DecisionChange) string {
	if len(changes) == 0 {
		return "无决策变更"
	}
	var lines []string
	for _, c := range changes {
		lines = append(lines, fmt.Sprintf("- %s: %s", c.Name, c.Detail))
	}
	return strings.Join(lines, "\n")
}
