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

// AlertService 告警服务：落库 + zap + urgent 立即发信 + 普通 level 1 小时去重。
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

// Raise 落一条告警（§11.6：落库 + zap；urgent 额外立即发信）。
// 非 urgent 同 code 在 1 小时内只落/发一次（去重）；urgent 不受限（验收 §10.5-7）。
func (a *AlertService) Raise(ctx context.Context, tradeDate, source string, level model.AlertLevel, code, title, content string) error {
	if !level.Valid() {
		return fmt.Errorf("非法告警级别: %q", level)
	}
	if level != model.AlertUrgent {
		last, err := a.st.OpsRepo().RecentAlertAt(ctx, code)
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
	nowStr := a.now().UTC().Format(time.RFC3339)
	if err := a.st.OpsRepo().RaiseAlert(ctx, model.AgentAlert{
		TradeDate: tradeDate, Source: source, Level: level, Code: code,
		Title: title, Content: content, CreatedAt: nowStr,
	}); err != nil {
		return err
	}
	observability.S().Infow("告警已落库", "code", code, "level", string(level), "date", tradeDate, "title", title)

	if level == model.AlertUrgent {
		// urgent 立即发信（M6）；发送失败显式返回，绝不静默
		subject, body := RenderM6(string(level), code, title, content)
		if a.mail != nil {
			if merr := a.mail.SendNow(ctx, tradeDate, model.MailM6, subject, body); merr != nil {
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
