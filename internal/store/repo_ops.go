package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
	"jingzhe-trader/internal/model"
)

// OpsRepo 运维域仓储：job_run / agent_alert / action_log / mail_outbox。
type OpsRepo struct {
	wdb *sqlx.DB
	rdb *sqlx.DB
}

// OpsRepo 返回运维域仓储。
func (s *Store) OpsRepo() *OpsRepo {
	return &OpsRepo{wdb: s.writeDB, rdb: s.readDB}
}

// UpsertJobRun 写入/更新任务执行记录（按 job_name+trade_date+attempt 幂等）。
func (r *OpsRepo) UpsertJobRun(ctx context.Context, j model.JobRun) error {
	const q = `INSERT INTO job_run (job_name, trade_date, attempt, status, duration_ms, error, artifacts, degradations, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_name, trade_date, attempt) DO UPDATE SET
			status=excluded.status, duration_ms=excluded.duration_ms, error=excluded.error,
			artifacts=excluded.artifacts, degradations=excluded.degradations, finished_at=excluded.finished_at`
	if _, err := r.wdb.ExecContext(ctx, q,
		j.JobName, j.TradeDate, j.Attempt, j.Status, j.DurationMs, j.Error, j.Artifacts, j.Degradations, j.StartedAt, j.FinishedAt,
	); err != nil {
		return fmt.Errorf("写入任务记录 %s 失败: %w", j.JobName, err)
	}
	return nil
}

// RaiseAlert 写入告警（落 agent_alert）。
func (r *OpsRepo) RaiseAlert(ctx context.Context, a model.AgentAlert) error {
	const q = `INSERT INTO agent_alert (trade_date, source, level, code, title, content, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'unread', ?)`
	if _, err := r.wdb.ExecContext(ctx, q,
		a.TradeDate, a.Source, string(a.Level), a.Code, a.Title, a.Content, a.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入告警 %s 失败: %w", a.Code, err)
	}
	return nil
}

// MarkAlertRead 标记告警已读（外部 agent 处理完毕后调用）。
func (r *OpsRepo) MarkAlertRead(ctx context.Context, id int64) error {
	const q = `UPDATE agent_alert SET status='read', read_at=? WHERE id=?`
	if _, err := r.wdb.ExecContext(ctx, q, time.Now().UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("标记告警已读失败: %w", err)
	}
	return nil
}

// InsertActionLog 写入审计日志（状态机流转/配置变更/回执/本金修正）。
func (r *OpsRepo) InsertActionLog(ctx context.Context, l model.ActionLog) error {
	const q = `INSERT INTO action_log (trade_date, actor, object_type, object_id, action, before_value, after_value, reason, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.wdb.ExecContext(ctx, q,
		l.TradeDate, l.Actor, l.ObjectType, l.ObjectID, l.Action, l.BeforeValue, l.AfterValue, l.Reason, l.CreatedAt,
	); err != nil {
		return fmt.Errorf("写入审计日志失败: %w", err)
	}
	return nil
}

// EnqueueMail 写入邮件发件箱（pending）。
func (r *OpsRepo) EnqueueMail(ctx context.Context, m model.MailOutbox) (int64, error) {
	const q = `INSERT INTO mail_outbox (trade_date, mail_type, subject, body, status, attempts, created_at)
		VALUES (?, ?, ?, ?, 'pending', 0, ?)`
	res, err := r.wdb.ExecContext(ctx, q, m.TradeDate, string(m.MailType), m.Subject, m.Body, m.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("写入邮件发件箱失败: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// UnreadAlerts 读取未读告警（供 MCP get_alerts 使用）。
func (r *OpsRepo) UnreadAlerts(ctx context.Context, tradeDate string) ([]model.AgentAlert, error) {
	var as []model.AgentAlert
	if err := r.rdb.SelectContext(ctx, &as,
		`SELECT id, trade_date, source, level, code, title, content, status, created_at, COALESCE(read_at,'') AS read_at
		 FROM agent_alert WHERE trade_date=? AND status='unread' ORDER BY id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取未读告警 %s 失败: %w", tradeDate, err)
	}
	return as, nil
}

// ListAlerts 读取指定交易日全部告警（含已读，供每日日志核查）。
func (r *OpsRepo) ListAlerts(ctx context.Context, tradeDate string) ([]model.AgentAlert, error) {
	var as []model.AgentAlert
	if err := r.rdb.SelectContext(ctx, &as,
		`SELECT id, trade_date, source, level, code, title, content, status, created_at, COALESCE(read_at,'') AS read_at
		 FROM agent_alert WHERE trade_date=? ORDER BY id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取告警 %s 失败: %w", tradeDate, err)
	}
	return as, nil
}

// ===================== Batch 4 扩展（T04）：mail_outbox / agent_alert / job_run =====================

// mailColumns 统一 COALESCE 可空列，避免 NULL 扫描进 string 失败。
const mailColumns = `id, trade_date, mail_type, subject, body, status, attempts,
	COALESCE(last_error,'') AS last_error, COALESCE(next_retry_at,'') AS next_retry_at,
	created_at, COALESCE(sent_at,'') AS sent_at`

// InsertMail 写入指定状态的邮件行（用于 mail.enabled=false 时的显式 failed 落库，验收 #5）。
func (r *OpsRepo) InsertMail(ctx context.Context, m model.MailOutbox) (int64, error) {
	const q = `INSERT INTO mail_outbox (trade_date, mail_type, subject, body, status, attempts, last_error, next_retry_at, created_at, sent_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	res, err := r.wdb.ExecContext(ctx, q,
		m.TradeDate, string(m.MailType), m.Subject, m.Body, m.Status, m.Attempts, m.LastError, m.NextRetryAt, m.CreatedAt, m.SentAt)
	if err != nil {
		return 0, fmt.Errorf("写入邮件行 %s 失败: %w", m.MailType, err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// GetMail 按 id 读取单封邮件。
func (r *OpsRepo) GetMail(ctx context.Context, id int64) (model.MailOutbox, error) {
	var m model.MailOutbox
	err := r.rdb.GetContext(ctx, &m, `SELECT `+mailColumns+` FROM mail_outbox WHERE id=?`, id)
	if err != nil {
		return m, fmt.Errorf("读取邮件 #%d 失败: %w", id, err)
	}
	return m, nil
}

// ListMailByDate 读取某交易日全部邮件行（日报/自检/CLI 用）。
func (r *OpsRepo) ListMailByDate(ctx context.Context, tradeDate string) ([]model.MailOutbox, error) {
	var ms []model.MailOutbox
	if err := r.rdb.SelectContext(ctx, &ms,
		`SELECT `+mailColumns+` FROM mail_outbox WHERE trade_date=? ORDER BY id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取邮件列表 %s 失败: %w", tradeDate, err)
	}
	return ms, nil
}

// PendingMails 读取到期待发邮件（status=pending 且重试时间已到）。
func (r *OpsRepo) PendingMails(ctx context.Context, now string) ([]model.MailOutbox, error) {
	var ms []model.MailOutbox
	if err := r.rdb.SelectContext(ctx, &ms,
		`SELECT `+mailColumns+` FROM mail_outbox
		 WHERE status='pending' AND (next_retry_at='' OR next_retry_at IS NULL OR next_retry_at<=?)
		 ORDER BY id`, now); err != nil {
		return nil, fmt.Errorf("读取待发邮件失败: %w", err)
	}
	return ms, nil
}

// UpdateMailSent 标记邮件已发送。
func (r *OpsRepo) UpdateMailSent(ctx context.Context, id int64, sentAt string) error {
	const q = `UPDATE mail_outbox SET status='sent', sent_at=?, last_error='', next_retry_at='' WHERE id=?`
	if _, err := r.wdb.ExecContext(ctx, q, sentAt, id); err != nil {
		return fmt.Errorf("标记邮件 #%d 已发送失败: %w", id, err)
	}
	return nil
}

// UpdateMailRetry 记录发送失败后的重试状态（attempts / last_error / next_retry_at）。
func (r *OpsRepo) UpdateMailRetry(ctx context.Context, id int64, attempts int, lastErr, nextRetryAt string) error {
	const q = `UPDATE mail_outbox SET attempts=?, last_error=?, next_retry_at=? WHERE id=?`
	if _, err := r.wdb.ExecContext(ctx, q, attempts, lastErr, nextRetryAt, id); err != nil {
		return fmt.Errorf("更新邮件 #%d 重试状态失败: %w", id, err)
	}
	return nil
}

// MarkMailFailed 终态失败（重试耗尽）。
func (r *OpsRepo) MarkMailFailed(ctx context.Context, id int64, attempts int, lastErr string) error {
	const q = `UPDATE mail_outbox SET status='failed', attempts=?, last_error=?, next_retry_at='' WHERE id=?`
	if _, err := r.wdb.ExecContext(ctx, q, attempts, lastErr, id); err != nil {
		return fmt.Errorf("标记邮件 #%d 失败终态: %w", id, err)
	}
	return nil
}

// RecentAlertAt 返回同 code 最近一条告警的 created_at（1 小时去重用），无记录返回空串。
func (r *OpsRepo) RecentAlertAt(ctx context.Context, code string) (string, error) {
	var s string
	err := r.rdb.GetContext(ctx, &s,
		`SELECT COALESCE(MAX(created_at),'') FROM agent_alert WHERE code=?`, code)
	if err != nil {
		return "", fmt.Errorf("查询告警 %s 最近时间失败: %w", code, err)
	}
	return s, nil
}

// JobAttempts 统计某任务某日的执行次数（含 running）。
func (r *OpsRepo) JobAttempts(ctx context.Context, jobName, tradeDate string) (int, error) {
	var n int
	err := r.rdb.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM job_run WHERE job_name=? AND trade_date=?`, jobName, tradeDate)
	if err != nil {
		return 0, fmt.Errorf("统计任务 %s 执行次数失败: %w", jobName, err)
	}
	return n, nil
}

// HasJobSucceeded 某任务某日是否已成功/降级完成（补跑判定：完成过即不重跑）。
func (r *OpsRepo) HasJobSucceeded(ctx context.Context, jobName, tradeDate string) (bool, error) {
	var n int
	err := r.rdb.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM job_run WHERE job_name=? AND trade_date=? AND status IN ('success','degraded')`,
		jobName, tradeDate)
	if err != nil {
		return false, fmt.Errorf("查询任务 %s 成功状态失败: %w", jobName, err)
	}
	return n > 0, nil
}

// LastJobAttemptAt 返回某任务某日最近一次尝试的 started_at（RFC3339 UTC），无记录返回空串。
func (r *OpsRepo) LastJobAttemptAt(ctx context.Context, jobName, tradeDate string) (string, error) {
	var s string
	err := r.rdb.GetContext(ctx, &s,
		`SELECT COALESCE(MAX(started_at),'') FROM job_run WHERE job_name=? AND trade_date=?`, jobName, tradeDate)
	if err != nil {
		return "", fmt.Errorf("查询任务 %s 最近尝试时间失败: %w", jobName, err)
	}
	return s, nil
}

// ListJobRuns 读取某交易日全部任务记录（日报分列 degraded 与 success 用）。
func (r *OpsRepo) ListJobRuns(ctx context.Context, tradeDate string) ([]model.JobRun, error) {
	var js []model.JobRun
	if err := r.rdb.SelectContext(ctx, &js,
		`SELECT id, job_name, trade_date, attempt, status, COALESCE(duration_ms,0) AS duration_ms,
		 COALESCE(error,'') AS error, COALESCE(artifacts,'') AS artifacts, COALESCE(degradations,'') AS degradations,
		 started_at, COALESCE(finished_at,'') AS finished_at
		 FROM job_run WHERE trade_date=? ORDER BY started_at, id`, tradeDate); err != nil {
		return nil, fmt.Errorf("读取任务记录 %s 失败: %w", tradeDate, err)
	}
	return js, nil
}
