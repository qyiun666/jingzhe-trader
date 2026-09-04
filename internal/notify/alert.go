package notify

import (
	"context"
	"fmt"
	"time"

	"jingzhe-trader/internal/model"
	"jingzhe-trader/internal/observability"
	"jingzhe-trader/internal/store"
)

// DedupWindow 同 code 告警去重窗口（1 小时，附录 A）。
const DedupWindow = time.Hour

// AlertService 告警服务：落库 + zap + urgent 立即发信。
//
// 两类去重：非 urgent 同一 code 1 小时内不再重复；urgent 轨迹每次刷新，但 M6 按 code 每天一封。
type AlertService struct {
	st   *store.Store
	mail *Mailer
	now  func() time.Time
}

// NewAlertService 构造告警服务。
func NewAlertService(st *store.Store, mail *Mailer) *AlertService {
	return &AlertService{st: st, mail: mail, now: time.Now}
}

// WithClock 注入时钟（测试用）。
func (a *AlertService) WithClock(f func() time.Time) *AlertService {
	if f != nil {
		a.now = f
	}
	return a
}

// Raise 落一条失败轨迹 + zap；urgent 额外立即发信。
//
// 告警不再有独立表：它是 run_trace 里一行 outcome=fail 的记录，subject=alert:<code>。
// 原来 agent_alert 的 level / source 两列没有读者参与判定（只有 urgent 这一条分支在代码里），
// 故折进 Detail 文本保留出处，不再占列。
func (a *AlertService) Raise(ctx context.Context, tradeDate, source string, level model.AlertLevel, code, title, content string) error {
	if !level.Valid() {
		return fmt.Errorf("非法告警级别: %q", level)
	}
	subject := model.TraceAlert(code)
	if level != model.AlertUrgent {
		last, err := a.st.TraceRepo().RecentFailAt(ctx, subject)
		if err != nil {
			return err // 去重查询失败显式上抛，不静默当无记录
		}
		if last != "" {
			if t, perr := time.Parse(time.RFC3339, last); perr == nil && a.now().Sub(t) < DedupWindow {
				observability.S().Infow("告警 1 小时内去重跳过", "code", code, "last_at", last)
				return nil
			}
		}
	}
	// urgent 不参与上面的 1 小时窗口（它该立刻响），但同一个 code 当天只发一封：
	// JOB_FAILED 这类反复触发的 code 每轮都 raise 一次，原来就会每轮发一封 M6，
	// 收件箱被同一件事刷爆，而轨迹行按 (trade_date, subject) 覆盖成一封都看不出来。
	// 轨迹照常刷新（保留最新原因与时间），只是不再重复投递。
	mailIt := true
	if level == model.AlertUrgent {
		last, err := a.st.TraceRepo().LastAt(ctx, subject, tradeDate)
		if err != nil {
			return err
		}
		mailIt = last == ""
	}
	detail := fmt.Sprintf("[%s/%s] %s — %s", source, level, title, content)
	if err := a.st.TraceRepo().Write(ctx, model.RunTrace{
		TradeDate: tradeDate, Subject: subject,
		Outcome: model.TraceFail, Detail: detail,
		At: a.now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	observability.S().Infow("告警已落轨迹", "code", code, "level", string(level), "date", tradeDate, "title", title)

	if level == model.AlertUrgent {
		if !mailIt {
			observability.S().Infow("urgent 告警当日已投递过该 code，跳过重复发信", "code", code, "date", tradeDate)
			return nil
		}
		// urgent 立即发信（M6），每个 code 当天一封；发送失败显式返回，绝不静默
		mSubject, mBody := RenderM6(string(level), code, title, content)
		if a.mail != nil {
			if merr := a.mail.Send(ctx, tradeDate, model.MailM6, mSubject, mBody); merr != nil {
				return fmt.Errorf("urgent 告警 %s 发信失败: %w", code, merr)
			}
		}
	}
	return nil
}

// RaiseUrgent 立即发信的紧急告警（不做 1 小时去重）。
func (a *AlertService) RaiseUrgent(ctx context.Context, tradeDate, source, code, title, content string) error {
	return a.Raise(ctx, tradeDate, source, model.AlertUrgent, code, title, content)
}
