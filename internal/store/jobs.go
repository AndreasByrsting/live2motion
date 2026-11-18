package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// 任务状态
const (
	JobPending = "pending"
	JobRunning = "running"
	JobSuccess = "success"
	JobFailed  = "failed"
	JobSkipped = "skipped" // dry-run 命中，未实际转换
)

// JobView 是任务列表页需要的聚合视图。
type JobView struct {
	JobID         int64
	PairID        int64
	ContentID     string
	HeicRelPath   string
	VideoRelPath  string
	HeicSize      int64
	VideoSize     int64
	Status        string
	Stage         string
	Progress      float64
	Attempts      int
	MaxAttempts   int
	LastError     string
	OutputRelPath string
	StartedAt     int64
	FinishedAt    int64
	CreatedAt     int64
	UpdatedAt     int64
}

// JobStats 仪表盘统计。
type JobStats struct {
	Pending int64
	Running int64
	Success int64
	Failed  int64
	Skipped int64
}

// Total 返回任务总数。
func (s JobStats) Total() int64 { return s.Pending + s.Running + s.Success + s.Failed + s.Skipped }

const jobViewSelect = `SELECT j.id, j.pair_id, p.content_id, p.heic_rel_path, p.video_rel_path,
	p.heic_size, p.video_size, j.status, COALESCE(j.stage,''), j.progress,
	j.attempts, j.max_attempts, COALESCE(j.last_error,''), COALESCE(o.rel_path,''),
	COALESCE(j.started_at,0), COALESCE(j.finished_at,0), j.created_at, j.updated_at
	FROM jobs j
	JOIN live_pairs p ON p.id = j.pair_id
	LEFT JOIN motion_outputs o ON o.pair_id = j.pair_id`

func scanJobView(row interface{ Scan(...any) error }) (JobView, error) {
	var v JobView
	err := row.Scan(&v.JobID, &v.PairID, &v.ContentID, &v.HeicRelPath, &v.VideoRelPath,
		&v.HeicSize, &v.VideoSize, &v.Status, &v.Stage, &v.Progress,
		&v.Attempts, &v.MaxAttempts, &v.LastError, &v.OutputRelPath,
		&v.StartedAt, &v.FinishedAt, &v.CreatedAt, &v.UpdatedAt)
	return v, err
}

// EnsureJob 为配对创建任务（已存在则不动），返回任务 id 与是否新建。
func (s *Store) EnsureJob(ctx context.Context, pairID int64, maxAttempts int) (int64, bool, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	now := Now()
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE pair_id=?`, pairID).Scan(&id)
	switch {
	case err == nil:
		return id, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs
		(pair_id, status, progress, attempts, max_attempts, created_at, updated_at)
		VALUES (?,?,0,0,?,?,?)`, pairID, JobPending, maxAttempts, now, now)
	if err != nil {
		// 并发扫描可能同时插入，唯一约束冲突时回读即可。
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			var existed int64
			if e2 := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE pair_id=?`, pairID).Scan(&existed); e2 == nil {
				return existed, false, nil
			}
		}
		return 0, false, err
	}
	id, _ = res.LastInsertId()
	return id, true, nil
}

// ClaimNextJob 原子领取一个可执行任务（状态置为 running 且 attempts+1）。
// 返回 nil 表示当前没有待处理任务。
func (s *Store) ClaimNextJob(ctx context.Context) (*JobView, error) {
	now := Now()
	var jobID, pairID int64
	err := s.db.QueryRowContext(ctx, `
		UPDATE jobs SET status=?, started_at=?, attempts=attempts+1, progress=0, stage='queued', updated_at=?
		WHERE id = (
			SELECT id FROM jobs
			WHERE status=? AND attempts < max_attempts
			  AND (next_retry_at IS NULL OR next_retry_at <= ?)
			ORDER BY id LIMIT 1
		)
		RETURNING id, pair_id`, JobRunning, now, now, JobPending, now).Scan(&jobID, &pairID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("领取任务失败: %w", err)
	}
	row := s.db.QueryRowContext(ctx, jobViewSelect+` WHERE j.id=?`, jobID)
	v, err := scanJobView(row)
	if err != nil {
		return nil, fmt.Errorf("读取任务详情失败: %w", err)
	}
	return &v, nil
}

// UpdateProgress 更新阶段与进度（0-1）。
func (s *Store) UpdateProgress(ctx context.Context, jobID int64, stage string, progress float64) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 1 {
		progress = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET stage=?, progress=?, updated_at=? WHERE id=?`,
		stage, progress, Now(), jobID)
	return err
}

// CompleteJob 标记成功。
func (s *Store) CompleteJob(ctx context.Context, jobID int64) error {
	now := Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, stage='done', progress=1, last_error=NULL, next_retry_at=NULL,
		 finished_at=?, updated_at=? WHERE id=?`, JobSuccess, now, now, jobID)
	return err
}

// SkipJob 标记跳过（dry-run）。
func (s *Store) SkipJob(ctx context.Context, jobID int64, reason string) error {
	now := Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, stage='dry-run', progress=1, last_error=NULLIF(?,''),
		 finished_at=?, updated_at=? WHERE id=?`, JobSkipped, reason, now, now, jobID)
	return err
}

// FailJob 记录一次失败：未达上限则回到 pending 并设置指数退避，达到上限则标记 failed。
func (s *Store) FailJob(ctx context.Context, jobID int64, errMsg string) (final bool, err error) {
	now := Now()
	var attempts, maxAttempts int
	if err := s.db.QueryRowContext(ctx,
		`SELECT attempts, max_attempts FROM jobs WHERE id=?`, jobID).Scan(&attempts, &maxAttempts); err != nil {
		return false, err
	}
	if attempts >= maxAttempts {
		_, err = s.db.ExecContext(ctx,
			`UPDATE jobs SET status=?, last_error=?, next_retry_at=NULL, finished_at=?, updated_at=? WHERE id=?`,
			JobFailed, errMsg, now, now, jobID)
		return true, err
	}
	backoff := int64(30 * (1 << (attempts - 1))) // 30s, 60s, 120s ...
	if backoff > 3600 {
		backoff = 3600
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, last_error=?, next_retry_at=?, updated_at=? WHERE id=?`,
		JobPending, errMsg, now+backoff, now, jobID)
	return false, err
}

// RecoverRunning 把上次进程崩溃时残留的 running 任务打回 pending。
func (s *Store) RecoverRunning(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, stage='recovered', progress=0, updated_at=? WHERE status=?`,
		JobPending, Now(), JobRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ResetJob 把任务重置为待处理（重新转换）。
func (s *Store) ResetJob(ctx context.Context, jobID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		status=?, stage=NULL, progress=0, attempts=0, last_error=NULL, next_retry_at=NULL,
		started_at=NULL, finished_at=NULL, updated_at=? WHERE id=?`, JobPending, Now(), jobID)
	return err
}

// ResetFailed 清空失败标记。retry=true 时一并重置重试次数以便重新执行；
// retry=false 只清除 failed 状态与错误信息，任务仍为 pending 等待下一轮。
func (s *Store) ResetFailed(ctx context.Context, retry bool) (int64, error) {
	if retry {
		res, err := s.db.ExecContext(ctx, `UPDATE jobs SET
			status=?, stage=NULL, progress=0, attempts=0, last_error=NULL, next_retry_at=NULL,
			started_at=NULL, finished_at=NULL, updated_at=? WHERE status=?`, JobPending, Now(), JobFailed)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET status=?, last_error=NULL, updated_at=? WHERE status=?`,
		JobPending, Now(), JobFailed)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ResetJobByPair 供 Web 单条「重新转换」使用。
func (s *Store) ResetJobByPair(ctx context.Context, pairID int64) error {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE pair_id=?`, pairID).Scan(&id); err != nil {
		return err
	}
	return s.ResetJob(ctx, id)
}

// ResetJobForChange 源文件指纹变化时重置任务，但只在任务已结束时生效，
// 避免打断正在进行的转换。返回受影响行数。
func (s *Store) ResetJobForChange(ctx context.Context, pairID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		status=?, stage='source-changed', progress=0, attempts=0, last_error=NULL,
		next_retry_at=NULL, started_at=NULL, finished_at=NULL, updated_at=?
		WHERE pair_id=? AND status IN (?,?,?)`,
		JobPending, Now(), pairID, JobSuccess, JobSkipped, JobFailed)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FailJobPermanent 立即标记失败且不再重试（源文件缺失等不可恢复错误）。
func (s *Store) FailJobPermanent(ctx context.Context, jobID int64, errMsg string) error {
	now := Now()
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET status=?, last_error=?, next_retry_at=NULL, attempts=max_attempts,
		 finished_at=?, updated_at=? WHERE id=?`, JobFailed, errMsg, now, now, jobID)
	return err
}

// JobFilter 任务列表筛选条件。
type JobFilter struct {
	Status string
	Query  string
	Limit  int
	Offset int
}

// ListJobs 分页查询任务，返回当前页与总数。
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]JobView, int64, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	where := []string{"1=1"}
	args := []any{}
	if f.Status != "" && f.Status != "all" {
		where = append(where, "j.status=?")
		args = append(args, f.Status)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(p.content_id LIKE ? OR p.heic_rel_path LIKE ? OR p.video_rel_path LIKE ?)")
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	cond := strings.Join(where, " AND ")

	var total int64
	countSQL := `SELECT COUNT(*) FROM jobs j JOIN live_pairs p ON p.id=j.pair_id WHERE ` + cond
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.db.QueryContext(ctx,
		jobViewSelect+` WHERE `+cond+` ORDER BY j.id DESC LIMIT ? OFFSET ?`, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]JobView, 0, f.Limit)
	for rows.Next() {
		v, err := scanJobView(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, total, rows.Err()
}

// GetJobView 读取单条任务视图。
func (s *Store) GetJobView(ctx context.Context, jobID int64) (JobView, error) {
	row := s.db.QueryRowContext(ctx, jobViewSelect+` WHERE j.id=?`, jobID)
	return scanJobView(row)
}

// Stats 聚合各状态任务数。
func (s *Store) JobStats(ctx context.Context) (JobStats, error) {
	var st JobStats
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return st, err
		}
		switch status {
		case JobPending:
			st.Pending = n
		case JobRunning:
			st.Running = n
		case JobSuccess:
			st.Success = n
		case JobFailed:
			st.Failed = n
		case JobSkipped:
			st.Skipped = n
		}
	}
	return st, rows.Err()
}

// SkipJobsForMissingAssets 把「源文件已被移走/删除」的任务标记为跳过。
// 这类任务再重试也只会失败（源路径不存在），标成 skipped 比 failed 更符合事实，
// 也避免仪表盘上一堆红色失败数字。
func (s *Store) SkipJobsForMissingAssets(ctx context.Context, sourceRoot string) (int64, error) {
	now := Now()
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET
		status=?, stage='source-missing', last_error=?, next_retry_at=NULL, finished_at=?, updated_at=?
		WHERE status IN (?,?)
		  AND pair_id IN (
			SELECT p.id FROM live_pairs p
			JOIN live_assets h ON h.id = p.heic_asset_id
			JOIN live_assets v ON v.id = p.video_asset_id
			WHERE p.source_root=? AND (h.state=? OR v.state=?)
		  )`,
		JobSkipped, "源文件已移除，跳过", now, now,
		JobPending, JobFailed,
		sourceRoot, AssetStateMissing, AssetStateMissing)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// HasRunnableJob 判断队列里是否还有可执行任务（含已到重试时间的）。
func (s *Store) HasRunnableJob(ctx context.Context) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs
		WHERE status=? AND attempts < max_attempts AND (next_retry_at IS NULL OR next_retry_at<=?)`,
		JobPending, Now()).Scan(&n)
	return n > 0, err
}
