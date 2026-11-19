package state

import (
	"context"
	"database/sql"
	"errors"
)

const (
	ReembedQueued     = "queued"
	ReembedProcessing = "processing"
	ReembedSucceeded  = "succeeded"
	ReembedFailed     = "failed"
)

var ErrReembedSourceChanged = errors.New("知识库配置已变化，请重新提交向量化任务")

// ReembedJob is the public status of an index rebuild.
type ReembedJob struct {
	ID        string `json:"job_id"`
	Namespace string `json:"namespace"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type ReembedTask struct {
	ReembedJob
	BaseVersion       int
	BaseCollection    string
	TargetSettings    map[string]any
	StagingCollection string
}

type ReembedJobs struct{ stores *Stores }

const reembedStatusColumns = "job_id, namespace, status, COALESCE(error, ''), created_at, updated_at"

func (j *ReembedJobs) Submit(ctx context.Context, namespace string, version int, collection string, settings map[string]any) (*ReembedJob, error) {
	id, err := uuid4String()
	if err != nil {
		return nil, err
	}
	encoded, err := marshalJSON(settings)
	if err != nil {
		return nil, err
	}
	now := nowISO()
	_, err = j.stores.DB.ExecContext(ctx, "INSERT INTO reembed_jobs "+
		"(job_id, namespace, active_namespace, base_version, base_collection, target_settings, staging_collection, status, created_at, updated_at) "+
		"VALUES (?, ?, ?, ?, ?, ?, '', ?, ?, ?)", id, namespace, namespace, version, collection, encoded, ReembedQueued, now, now)
	if isUniqueViolation(err) {
		return nil, newStateConflict("该知识库已有重新向量化任务在排队或执行")
	}
	if err != nil {
		return nil, err
	}
	return &ReembedJob{ID: id, Namespace: namespace, Status: ReembedQueued, CreatedAt: now, UpdatedAt: now}, nil
}

func (j *ReembedJobs) Latest(ctx context.Context, namespace string) (*ReembedJob, error) {
	var job ReembedJob
	err := j.stores.DB.QueryRowContext(ctx, "SELECT "+reembedStatusColumns+" FROM reembed_jobs "+
		"WHERE namespace = ? ORDER BY created_at DESC, job_id DESC LIMIT 1", namespace).
		Scan(&job.ID, &job.Namespace, &job.Status, &job.Error, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &job, err
}

// Pending reads the prepared settings only for jobs that still need execution.
func (j *ReembedJobs) Pending(ctx context.Context, namespace, id string) (*ReembedTask, error) {
	task := ReembedTask{ReembedJob: ReembedJob{ID: id, Namespace: namespace}}
	var raw string
	err := j.stores.DB.QueryRowContext(ctx, "SELECT base_version, base_collection, target_settings, staging_collection "+
		"FROM reembed_jobs WHERE job_id = ? AND active_namespace = ?", id, namespace).
		Scan(&task.BaseVersion, &task.BaseCollection, &raw, &task.StagingCollection)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	task.TargetSettings, err = decodeObject(raw)
	return &task, err
}

func (j *ReembedJobs) Start(ctx context.Context, token *LockToken, task *ReembedTask, staging string) error {
	return j.withFence(ctx, token, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE reembed_jobs SET status = ?, staging_collection = ?, updated_at = ? "+
			"WHERE job_id = ? AND active_namespace = ?", ReembedProcessing, staging, nowISO(), task.ID, task.Namespace)
		return err
	})
}

func (j *ReembedJobs) Fail(ctx context.Context, token *LockToken, task *ReembedTask, message string) error {
	return j.withFence(ctx, token, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE reembed_jobs SET status = ?, error = ?, "+
			"active_namespace = NULL, target_settings = NULL, updated_at = ? WHERE job_id = ? AND active_namespace = ?",
			ReembedFailed, message, nowISO(), task.ID, task.Namespace)
		return err
	})
}

// Publish switches the index and finishes the job in one transaction. After an
// ambiguous commit, replay can inspect the job before touching its collection.
func (j *ReembedJobs) Publish(ctx context.Context, token *LockToken, task *ReembedTask, settings map[string]any) error {
	encoded, err := marshalJSON(settings)
	if err != nil {
		return err
	}
	return j.withFence(ctx, token, func(tx *sql.Tx) error {
		now := nowISO()
		if _, err := touch(ctx, tx, knowledgeKeys(task.Namespace)...); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE knowledge_bases SET settings = ?, collection = ?, version = version + 1, updated_at = ? "+
			"WHERE namespace = ? AND version = ? AND collection = ?", encoded, task.StagingCollection, now,
			task.Namespace, task.BaseVersion, task.BaseCollection)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return ErrReembedSourceChanged
		}
		_, err = tx.ExecContext(ctx, "UPDATE reembed_jobs SET status = ?, active_namespace = NULL, target_settings = NULL, "+
			"updated_at = ? WHERE job_id = ? AND active_namespace = ?", ReembedSucceeded, now, task.ID, task.Namespace)
		return err
	})
}

func (j *ReembedJobs) withFence(ctx context.Context, token *LockToken, apply func(*sql.Tx) error) error {
	tx, err := j.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := j.stores.Uploads.AssertLockTx(ctx, tx, token); err != nil {
		return err
	}
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}
