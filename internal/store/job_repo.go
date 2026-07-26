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
	now := time.Now().Format("2006-01-02 15:04:05")
	res, err := r.db.Exec(`INSERT INTO job_run (job_name, trade_date, status, error, started_at, finished_at)
		VALUES (?, ?, ?, '', ?, '')`, jobName, tradeDate, JobStatusRunning, now)
	if err != nil {
		return 0, fmt.Errorf("记录任务开始失败(%s): %w", jobName, err)
	}
	return res.LastInsertId()
}

// FinishJob 记录任务结束
func (r *JobRepo) FinishJob(id int64, status, errMsg string) error {
	now := time.Now().Format("2006-01-02 15:04:05")
	if _, err := r.db.Exec(`UPDATE job_run SET status = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, errMsg, now, id); err != nil {
		return fmt.Errorf("记录任务结束失败(id=%d): %w", id, err)
	}
	return nil
}

// HasSucceeded 判断指定任务当日是否已成功执行 (防重复执行/启动补跑判断)
func (r *JobRepo) HasSucceeded(jobName, tradeDate string) (bool, error) {
	var count int
	err := r.db.Get(&count, `SELECT COUNT(1) FROM job_run WHERE job_name = ? AND trade_date = ? AND status = ?`,
		jobName, tradeDate, JobStatusSuccess)
	if err != nil {
		return false, fmt.Errorf("查询任务记录失败(%s): %w", jobName, err)
	}
	return count > 0, nil
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
