package agent

import (
	"fmt"

	"jingzhe-trader/internal/store"
)

// ReviewWindowDays 辩论结论复盘窗口: 决策满 N 个自然日后回填实际收益
// (自然日窗口内若含停牌则该条留待下次, 不产生错误数据)
const ReviewWindowDays = 5

// evaluateDecision 判定决策正确性: buy→涨为对; sell/reject→跌为对
func evaluateDecision(decision string, retPct float64) int {
	switch decision {
	case "buy":
		if retPct > 0 {
			return 1
		}
	case "sell", "reject":
		if retPct < 0 {
			return 1
		}
	}
	return 0
}

// ReviewDebates 回填待复盘辩论结论的实际收益:
// base_close=决策日收盘, last_close=截至 asOf 的最新收盘, ret_pct=区间涨跌幅
// 单条数据缺失 (决策日无K线/复盘窗口内无新K线即停牌) 跳过, 留待下次复盘
// 返回本次新回填的记录
func ReviewDebates(debateRepo *store.DebateRepo, reviewRepo *store.DebateReviewRepo,
	barRepo *store.BarRepo, asOf string) ([]*store.DebateReview, error) {

	pending, err := debateRepo.GetPendingReview(dateMinusDays(asOf, ReviewWindowDays), 0)
	if err != nil {
		return nil, err
	}
	reviewed := make([]*store.DebateReview, 0, len(pending))
	for i := range pending {
		d := &pending[i]
		baseBars, err := barRepo.GetBars(d.TsCode, d.TradeDate, d.TradeDate)
		if err != nil || len(baseBars) == 0 {
			continue
		}
		history, err := barRepo.GetBars(d.TsCode, d.TradeDate, asOf)
		if err != nil || len(history) == 0 {
			continue
		}
		last := history[len(history)-1]
		if last.TradeDate == d.TradeDate {
			continue // 窗口内无新K线 (停牌/未同步), 留待下次
		}
		base := baseBars[len(baseBars)-1].Close
		retPct := 0.0
		if base > 0 {
			retPct = (last.Close - base) / base * 100
		}
		rv := &store.DebateReview{
			DebateID: d.ID, TradeDate: d.TradeDate, TsCode: d.TsCode,
			Decision: d.Decision, Confidence: d.Confidence,
			BaseClose: base, ReviewDate: asOf, LastClose: last.Close,
			RetPct: retPct, Correct: evaluateDecision(d.Decision, retPct),
		}
		if _, err := reviewRepo.Insert(rv); err != nil {
			return reviewed, fmt.Errorf("复盘落库失败 ts_code=%s debate_id=%d: %w", d.TsCode, d.ID, err)
		}
		reviewed = append(reviewed, rv)
	}
	return reviewed, nil
}
