package store

import (
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// 任务执行状态
const (
	JobStatusRunning = "running"
	JobStatusSuccess = "success"
	JobStatusFailed  = "failed"
)

// 任务名常量 (job_run 表 / 健康度展示 / API 共用, 避免各包硬编码字面量)
const (
	JobDataUpdate   = "data_update"
	JobScreener     = "screener"
	JobSignal       = "signal"
	JobReconcile    = "reconcile"
	JobReport       = "report"
	JobIntraday     = "intraday_monitor"
	JobRetention    = "retention"
	JobSettleT1     = "settle_t1"
	JobPremarket    = "premarket"
	JobDebateReview = "debate_review"
)

// JobNames 全部任务名列表 (健康度/变更报告展示用, 新增任务时同步追加)
var JobNames = []string{JobDataUpdate, JobScreener, JobSignal, JobReconcile, JobReport, JobIntraday, JobRetention, JobSettleT1, JobPremarket, JobDebateReview}

// JobRun 调度任务执行记录
type JobRun struct {
	ID         int64  `json:"id" db:"id"`
	JobName    string `json:"job_name" db:"job_name"`
	TradeDate  string `json:"trade_date" db:"trade_date"`
	Status     string `json:"status" db:"status"`
	Error      string `json:"error" db:"error"`
	StartedAt  string `json:"started_at" db:"started_at"`
	FinishedAt string `json:"finished_at" db:"finished_at"`
}

// JobRepo 调度任务记录仓储
type JobRepo struct {
	db *sqlx.DB
}

// NewJobRepo 创建任务记录仓储
func NewJobRepo(db *sqlx.DB) *JobRepo {
	return &JobRepo{db: db}
}

// StartJob 记录任务开始, 返回记录ID
func (r *JobRepo) StartJob(jobName, tradeDate string) (int64, error) {
	now := time.Now().Format(TimeLayout)
	res, err := r.db.Exec(`INSERT INTO job_run (job_name, trade_date, status, error, started_at, finished_at)
		VALUES (?, ?, ?, '', ?, '')`, jobName, tradeDate, JobStatusRunning, now)
	if err != nil {
		return 0, fmt.Errorf("记录任务开始失败(%s): %w", jobName, err)
	}
	return res.LastInsertId()
}

// FinishJob 记录任务结束
func (r *JobRepo) FinishJob(id int64, status, errMsg string) error {
	now := time.Now().Format(TimeLayout)
	if _, err := r.db.Exec(`UPDATE job_run SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errMsg, now, id); err != nil {
		return fmt.Errorf("记录任务结束失败(id=%d): %w", id, err)
	}
	return nil
}

// HasSucceeded 判断指定任务当日是否已成功执行 (防重复执行/启动补跑判断)
func (r *JobRepo) HasSucceeded(jobName, tradeDate string) (bool, error) {
	return existsRow(r.db, `SELECT COUNT(1) FROM job_run WHERE job_name = ? AND trade_date = ? AND status = ?`,
		jobName, tradeDate, JobStatusSuccess)
}

// LastAttemptStartedAt 返回指定任务当日最后一次尝试的开始时间 (用于重试间隔判断)
// 无记录时返回零值和 nil error
func (r *JobRepo) LastAttemptStartedAt(jobName, tradeDate string) (time.Time, error) {
	var startedAt string
	err := r.db.Get(&startedAt, `SELECT started_at FROM job_run
		WHERE job_name = ? AND trade_date = ?
		ORDER BY id DESC LIMIT 1`, jobName, tradeDate)
	if err != nil {
		if isNoRowsErr(err) {
			return time.Time{}, nil // 无记录不算错误
		}
		return time.Time{}, fmt.Errorf("查询任务尝试时间失败(%s): %w", jobName, err)
	}
	t, err := time.ParseInLocation(TimeLayout, startedAt, time.Local)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// LastSuccess 查询任务最近一次成功记录 (健康度展示)
func (r *JobRepo) LastSuccess(jobName string) (*JobRun, error) {
	var run JobRun
	err := r.db.Get(&run, `SELECT * FROM job_run WHERE job_name = ? AND status = ? ORDER BY id DESC LIMIT 1`,
		jobName, JobStatusSuccess)
	if err != nil {
		return nil, fmt.Errorf("查询任务成功记录失败(%s): %w", jobName, err)
	}
	return &run, nil
}
