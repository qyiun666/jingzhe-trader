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
	Code   string // 失败时的告警码（如 MAIL_NOT_SENT / ARTIFACT_MISSING）
	Detail string
}

// BuildDailyChecks 六类产出物自检（日报的"当天失败汇报"）：
// 日历覆盖 / 日线行数 / 每日指标行数 / 待买卖表 / 日报邮件 / 任务状态。
// 只看结果表与当日轨迹 —— 选股条数、漏斗诊断、信号明细属过程产物，只写日志，不入库也不在此复算。
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
	basicRows, err := st.MarketRepo().CountValuation(ctx, date)
	if err != nil || basicRows == 0 {
		rs = append(rs, CheckResult{Name: "估值截面", OK: false, Code: "DATA_STALE",
			Detail: fmt.Sprintf("stock_basic 当日估值 0 行 err=%v", err)})
	} else {
		rs = append(rs, CheckResult{Name: "每日指标", OK: true, Detail: fmt.Sprintf("%d 行", basicRows)})
	}

	// 4. 待买卖表（0 条不判失败：无信号是合法结果，17:00 会自行跳过发信；
	//    选股条数与漏斗诊断只写日志，不在结果表里）
	tks, err := st.TradeRepo().ListTickets(ctx, date, "")
	if err != nil {
		rs = append(rs, CheckResult{Name: "待买卖表", OK: false, Code: "ARTIFACT_MISSING", Detail: err.Error()})
	} else {
		rs = append(rs, CheckResult{Name: "待买卖表", OK: true, Detail: fmt.Sprintf("%d 张指令单", len(tks))})
	}

	// 5/6. 一次读当日轨迹，供「日报邮件」与「任务状态」两项判定
	traces, terr := st.TraceRepo().List(ctx, date)
	if terr != nil {
		rs = append(rs, CheckResult{Name: "日报邮件", OK: false, Code: "MAIL_NOT_SENT", Detail: terr.Error()})
		rs = append(rs, CheckResult{Name: "任务记录", OK: false, Code: "JOB_FAILED", Detail: terr.Error()})
		return rs
	}

	// 5. M5 日报邮件（缺行或 fail → MAIL_NOT_SENT，验收 §10.5-11）
	MAIL_NOT_SENT := "MAIL_NOT_SENT"
	m5 := findTrace(traces, model.TraceMail(model.MailM5))
	switch {
	case m5 == nil:
		rs = append(rs, CheckResult{Name: "日报邮件", OK: false, Code: MAIL_NOT_SENT,
			Detail: "当日轨迹无 mail:M5 行（日报未执行或进程中途退出）"})
	case m5.Outcome == model.TraceFail:
		rs = append(rs, CheckResult{Name: "日报邮件", OK: false, Code: MAIL_NOT_SENT,
			Detail: "M5 发送失败：" + m5.Detail})
	default:
		rs = append(rs, CheckResult{Name: "日报邮件", OK: true, Detail: m5.Detail})
	}

	// 6. 任务轨迹（fail/partial 列名，供日报分列）
	var failed, partial []string
	for _, t := range traces {
		if !strings.HasPrefix(t.Subject, "job:") {
			continue
		}
		name := strings.TrimPrefix(t.Subject, "job:")
		switch t.Outcome {
		case model.TraceFail:
			failed = append(failed, name)
		case model.TracePartial:
			partial = append(partial, name)
		}
	}
	switch {
	case len(failed) > 0:
		rs = append(rs, CheckResult{Name: "任务记录", OK: false, Code: "JOB_FAILED",
			Detail: fmt.Sprintf("失败任务：%s", strings.Join(failed, ","))})
	case len(partial) > 0:
		rs = append(rs, CheckResult{Name: "任务记录", OK: true,
			Detail: fmt.Sprintf("降级任务：%s", strings.Join(partial, ","))})
	default:
		rs = append(rs, CheckResult{Name: "任务记录", OK: true, Detail: "全部成功"})
	}
	return rs
}

// BuildDailyBlock 渲染日报自检区块（M5 正文组成，验收 §10.5-11 的"日报自检区块"）。
func BuildDailyBlock(ctx context.Context, st *store.Store, date string, minBarRows int) string {
	rs := BuildDailyChecks(ctx, st, date, minBarRows)
	var b strings.Builder
	b.WriteString("== 静默失败自检（6 项）==\n")
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

// findTrace 取当日轨迹里指定 subject 的那一行。
func findTrace(ts []model.RunTrace, subject string) *model.RunTrace {
	for i := range ts {
		if ts[i].Subject == subject {
			return &ts[i]
		}
	}
	return nil
}
