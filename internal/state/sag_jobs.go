package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

const (
	sagExtractionMaxFailures  = 3
	sagExtractionLeaseSeconds = 180
)

type SagExtractionJob struct {
	Namespace  string
	DocumentID string
	Version    string
	ChunkID    string
	generation int64
	failures   int
}

func clearSagDocument(ctx context.Context, tx *sql.Tx, namespace, documentID string) error {
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM sag_extraction_jobs WHERE namespace = ? AND document_id = ?", namespace, documentID); err != nil {
		return err
	}
	return deleteSagEvents(ctx, tx, namespace, documentID)
}

// QueueExtraction replaces the document's SAG work in the same transaction
// that publishes its head. Qdrant keeps the actual fragment text.
func (r *SagRepository) QueueExtraction(ctx context.Context, tx *sql.Tx, namespace, documentID, version string, chunkIDs []string) error {
	if err := clearSagDocument(ctx, tx, namespace, documentID); err != nil {
		return err
	}
	if len(chunkIDs) == 0 {
		return nil
	}
	var enabled bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM model_profiles WHERE kind = 'extraction')").Scan(&enabled); err != nil || !enabled {
		return err
	}

	values := make([]string, 0, len(chunkIDs))
	args := make([]any, 0, 4*len(chunkIDs))
	for _, chunkID := range chunkIDs {
		values = append(values, "(?, ?, ?, ?, UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6)))")
		args = append(args, namespace, documentID, version, chunkID)
	}
	_, err := tx.ExecContext(ctx,
		"INSERT INTO sag_extraction_jobs (namespace, document_id, version, chunk_id, ready_at) VALUES "+
			strings.Join(values, ", "), args...)
	return err
}

// ClaimExtraction leases the next due chunk. ready_at also serves as the lease
// deadline, so an abandoned claim becomes eligible after the deadline.
func (r *SagRepository) ClaimExtraction(ctx context.Context) (*SagExtractionJob, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var job SagExtractionJob
	err = tx.QueryRowContext(ctx, "SELECT namespace, document_id, version, chunk_id, lease_generation, failures "+
		"FROM sag_extraction_jobs WHERE ready_at <= UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6)) "+
		"ORDER BY ready_at, namespace, document_id, version, chunk_id LIMIT 1 FOR UPDATE SKIP LOCKED").
		Scan(&job.Namespace, &job.DocumentID, &job.Version, &job.ChunkID, &job.generation, &job.failures)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	job.generation++
	if _, err := tx.ExecContext(ctx,
		"UPDATE sag_extraction_jobs SET lease_generation = ?, ready_at = UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6)) + ? "+
			"WHERE namespace = ? AND document_id = ? AND version = ? AND chunk_id = ?",
		job.generation, sagExtractionLeaseSeconds, job.Namespace, job.DocumentID, job.Version, job.ChunkID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

// ExtractionCollection resolves the current collection, including a collection
// switched by re-embedding since the chunk was queued. An empty collection
// means the document no longer has the requested published version.
func (r *SagRepository) ExtractionCollection(ctx context.Context, job *SagExtractionJob) (string, error) {
	var collection string
	err := r.db.QueryRowContext(ctx, "SELECT k.collection FROM knowledge_bases k "+
		"JOIN documents d ON d.namespace = k.namespace "+
		"JOIN document_heads h ON h.namespace = d.namespace AND h.document_id = d.document_id "+
		"WHERE d.namespace = ? AND d.document_id = ? AND d.del_flag = 0 AND h.version = ?",
		job.Namespace, job.DocumentID, job.Version).Scan(&collection)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return collection, err
}

// FinishExtraction records a result or schedules a failed chunk for retry.
// The lease generation fences late attempts. Lock the live document before its
// head and the job, in the same order as publication, replacement and deletion.
func (r *SagRepository) FinishExtraction(ctx context.Context, job *SagExtractionJob, extraction *SagExtraction, failure string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	currentVersion, active, err := lockSagDocumentVersion(ctx, tx, job.Namespace, job.DocumentID)
	if err != nil {
		return err
	}
	if !active || currentVersion != job.Version || (extraction == nil && failure == "") {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sag_extraction_jobs "+
			"WHERE namespace = ? AND document_id = ? AND version = ? AND chunk_id = ? AND lease_generation = ?",
			job.Namespace, job.DocumentID, job.Version, job.ChunkID, job.generation); err != nil {
			return err
		}
		return tx.Commit()
	}

	failures := job.failures
	var retrySeconds, lastError any
	if failure != "" {
		failures++
		lastError = failure
		if failures < sagExtractionMaxFailures {
			retrySeconds = 30 * failures
		}
	}
	result, err := tx.ExecContext(ctx, "UPDATE sag_extraction_jobs "+
		"SET failures = ?, last_error = ?, ready_at = UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6)) + ? "+
		"WHERE namespace = ? AND document_id = ? AND version = ? AND chunk_id = ? "+
		"AND lease_generation = ? AND ready_at > UNIX_TIMESTAMP(CURRENT_TIMESTAMP(6))",
		failures, lastError, retrySeconds, job.Namespace, job.DocumentID, job.Version, job.ChunkID, job.generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if extraction != nil {
		if err := insertSagExtraction(ctx, tx, job.Namespace, job.DocumentID, job.Version, nowSag(), 0, *extraction); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func lockSagDocumentVersion(ctx context.Context, tx *sql.Tx, namespace, documentID string) (string, bool, error) {
	var deleted bool
	err := tx.QueryRowContext(ctx, "SELECT del_flag FROM documents WHERE namespace = ? AND document_id = ? FOR UPDATE",
		namespace, documentID).Scan(&deleted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil || deleted {
		return "", false, err
	}
	var version string
	err = tx.QueryRowContext(ctx, "SELECT version FROM document_heads WHERE namespace = ? AND document_id = ? FOR UPDATE",
		namespace, documentID).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", true, nil // legacy unversioned document
	}
	return version, true, err
}
