package scheduler

import (
	"context"
	"fmt"
	"strings"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/store"
)

// CheckResult 单项自检结果。
type CheckResult struct {
	Name   string
	OK     bool
	Code   string // 失败时的告警码（如 MAIL_NOT_SENT / SNAPSHOT_MISSING）
	Detail string
}

// BuildDailyChecks 八类产出物自检（§5.1 20:00 / D1）：
// 日历覆盖 / 日线行数 / 每日指标行数 / 选股结果 / 信号 / 账户快照 / M5 邮件 / 任务状态。
// 纯读库，无副作用。
func BuildDailyChecks(ctx context.Context, st *store.Store, date string, minBarRows int) []CheckResult {
	var rs []CheckResult

	// 1. 交易日历覆盖未来 ≥ 30 天
	future, err := st.MarketRepo().CountFutureTradeDays(ctx, date)
	if err != nil {
		rs = append(rs, CheckResult{Name: "交易日历覆盖", OK: false, Code: "CAL_MISSING", Detail: err.Error()})
	} else if future < 30 {
		rs = append(rs, CheckResult{Name: "交易日历覆盖", OK: false, Code: "CAL_MISSING",
			Detail: fmt.Sprintf("未来交易日仅 %d 天（<30），需续拉日历", future)})
	} else {
		rs = append(rs, CheckResult{Name: "交易日历覆盖", OK: true, Detail: fmt.Sprintf("未来 %d 个交易日", future)})
	}

	// 2/3. 当日行情数据
	barRows, err := st.MarketRepo().CountBar(ctx, date)
	if err != nil || barRows < minBarRows {
		rs = append(rs, CheckResult{Name: "日线行情", OK: false, Code: "DATA_STALE",
			Detail: fmt.Sprintf("daily_bar %d 行（期望 ≥%d）err=%v", barRows, minBarRows, err)})
	} else {
		rs = append(rs, CheckResult{Name: "日线行情", OK: true, Detail: fmt.Sprintf("%d 行", barRows)})
	}
	basicRows, err := st.MarketRepo().CountDailyBasic(ctx, date)
	if err != nil || basicRows == 0 {
		rs = append(rs, CheckResult{Name: "每日指标", OK: false, Code: "DATA_STALE", Detail: fmt.Sprintf("daily_basic 0 行 err=%v", err)})
	} else {
		rs = append(rs, CheckResult{Name: "每日指标", OK: true, Detail: fmt.Sprintf("%d 行", basicRows)})
	}

	// 4. 选股结果（0 条本身不判失败——SCREEN_EMPTY 已有独立告警，此处只报告）
	cands, err := st.ScreenRepo().ListScreenResults(ctx, date)
	if err != nil {
		rs = append(rs, CheckResult{Name: "选股结果", OK: false, Code: "ARTIFACT_MISSING", Detail: err.Error()})
	} else {
		rs = append(rs, CheckResult{Name: "选股结果", OK: true, Detail: fmt.Sprintf("%d 条", len(cands))})
	}

	// 5. 信号
	sigN, err := st.DecisionRepo().CountSignals(ctx, date)
	if err != nil {
		rs = append(rs, CheckResult{Name: "信号", OK: false, Code: "ARTIFACT_MISSING", Detail: err.Error()})
	} else {
		rs = append(rs, CheckResult{Name: "信号", OK: true, Detail: fmt.Sprintf("%d 条", sigN)})
	}

	// 6. 账户快照
	hasSn, err := st.TradeRepo().HasSnapshot(ctx, date)
	if err != nil || !hasSn {
		rs = append(rs, CheckResult{Name: "账户快照", OK: false, Code: "SNAPSHOT_MISSING", Detail: fmt.Sprintf("当日无快照 err=%v", err)})
	} else {
		rs = append(rs, CheckResult{Name: "账户快照", OK: true, Detail: "已写入"})
	}

	// 7. M5 日报邮件（当日应有邮件无 sent 记录 → MAIL_NOT_SENT，验收 §10.5-11）
	MAIL_NOT_SENT := "MAIL_NOT_SENT"
	mails, err := st.OpsRepo().ListMailByDate(ctx, date)
	if err != nil {
		rs = append(rs, CheckResult{Name: "日报邮件", OK: false, Code: MAIL_NOT_SENT, Detail: err.Error()})
	} else {
		m5 := findMail(mails, model.MailM5)
		switch {
		case m5 == nil:
			rs = append(rs, CheckResult{Name: "日报邮件", OK: false, Code: MAIL_NOT_SENT,
				Detail: "当日 mail_outbox 无 M5 记录（日报未入队或被删除）"})
		case m5.Status == "failed":
			rs = append(rs, CheckResult{Name: "日报邮件", OK: false, Code: MAIL_NOT_SENT,
				Detail: "M5 终态 failed：" + m5.LastError})
		default:
			rs = append(rs, CheckResult{Name: "日报邮件", OK: true, Detail: "M5 状态 " + m5.Status})
		}
	}

	// 8. 任务状态（failed/degraded 列名，供日报分列）
	runs, err := st.OpsRepo().ListJobRuns(ctx, date)
	if err != nil {
		rs = append(rs, CheckResult{Name: "任务记录", OK: false, Code: "JOB_FAILED", Detail: err.Error()})
	} else {
		var failed, degraded []string
		for _, j := range runs {
			switch model.JobStatus(j.Status) {
			case model.JobFailed:
				failed = append(failed, j.JobName)
			case model.JobDegraded:
				degraded = append(degraded, j.JobName)
			}
		}
		if len(failed) > 0 {
			rs = append(rs, CheckResult{Name: "任务记录", OK: false, Code: "JOB_FAILED",
				Detail: fmt.Sprintf("失败任务：%s", strings.Join(failed, ","))})
		} else if len(degraded) > 0 {
			rs = append(rs, CheckResult{Name: "任务记录", OK: true,
				Detail: fmt.Sprintf("降级任务：%s", strings.Join(degraded, ","))})
		} else {
			rs = append(rs, CheckResult{Name: "任务记录", OK: true, Detail: "全部成功"})
		}
	}
	return rs
}

// BuildDailyBlock 渲染日报自检区块（M5 正文组成，验收 §10.5-11 的"日报自检区块"）。
func BuildDailyBlock(ctx context.Context, st *store.Store, date string, minBarRows int) string {
	rs := BuildDailyChecks(ctx, st, date, minBarRows)
	var b strings.Builder
	b.WriteString("== 静默失败自检（8 项）==\n")
	for _, r := range rs {
		mark := "✓"
		if !r.OK {
			mark = "✗"
		}
		fmt.Fprintf(&b, "%s %s：%s", mark, r.Name, r.Detail)
		if r.Code != "" {
			fmt.Fprintf(&b, " [%s]", r.Code)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// findMail 取指定类型的第一封邮件。
func findMail(ms []model.MailOutbox, t model.MailType) *model.MailOutbox {
	for i := range ms {
		if ms[i].MailType == t {
			return &ms[i]
		}
	}
	return nil
}
